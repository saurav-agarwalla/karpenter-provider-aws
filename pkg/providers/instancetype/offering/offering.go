/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package offering

import (
	"context"
	"fmt"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	awscache "github.com/aws/karpenter-provider-aws/pkg/cache"
	"github.com/aws/karpenter-provider-aws/pkg/providers/capacityreservation"
	"github.com/aws/karpenter-provider-aws/pkg/providers/pricing"
)

type Provider interface {
	InjectOfferings(context.Context, []*cloudprovider.InstanceType, *v1.EC2NodeClass, []string) []*cloudprovider.InstanceType
}

type NodeClass interface {
	CapacityReservations() []v1.CapacityReservation
	ZoneInfo() []v1.ZoneInfo
}

type DefaultProvider struct {
	pricingProvider             pricing.Provider
	capacityReservationProvider capacityreservation.Provider
	unavailableOfferings        *awscache.UnavailableOfferings
	cache                       *cache.Cache
}

func NewDefaultProvider(
	pricingProvider pricing.Provider,
	capacityReservationProvider capacityreservation.Provider,
	unavailableOfferingsCache *awscache.UnavailableOfferings,
	offeringCache *cache.Cache,
) *DefaultProvider {
	return &DefaultProvider{
		pricingProvider:             pricingProvider,
		capacityReservationProvider: capacityReservationProvider,
		unavailableOfferings:        unavailableOfferingsCache,
		cache:                       offeringCache,
	}
}

func (p *DefaultProvider) InjectOfferings(
	ctx context.Context,
	instanceTypes []*cloudprovider.InstanceType,
	nodeClass NodeClass,
	allZones sets.Set[string],
) []*cloudprovider.InstanceType {
	subnetZonesToZoneIDs := lo.SliceToMap(nodeClass.ZoneInfo(), func(info v1.ZoneInfo) (string, string) {
		return info.Zone, info.ZoneID
	})
	var its []*cloudprovider.InstanceType
	for _, it := range instanceTypes {
		offerings := p.createOfferings(
			ctx,
			it,
			nodeClass,
			allZones,
			subnetZonesToZoneIDs,
		)
		// NOTE: By making this copy one level deep, we can modify the offerings without mutating the results from previous
		// GetInstanceTypes calls. This should still be done with caution - it is currently done here in the provider, and
		// once in the instance provider (filterReservedInstanceTypes)
		its = append(its, &cloudprovider.InstanceType{
			Name:         it.Name,
			Requirements: it.Requirements,
			Offerings:    offerings,
			Capacity:     it.Capacity,
			Overhead:     it.Overhead,
		})
	}
	return its
}

//nolint:gocyclo
func (p *DefaultProvider) createOfferings(
	ctx context.Context,
	it *cloudprovider.InstanceType,
	nodeClass NodeClass,
	allZones sets.Set[string],
	subnetZonesToZoneIDs map[string]string,
) cloudprovider.Offerings {
	var offerings []*cloudprovider.Offering
	itZones := sets.New(it.Requirements.Get(corev1.LabelTopologyZone).Values()...)

	if ofs, ok := p.cache.Get(p.cacheKeyFromInstanceType(it)); ok {
		offerings = append(offerings, ofs.([]*cloudprovider.Offering)...)
	} else {
		var cachedOfferings []*cloudprovider.Offering
		for zone := range allZones {
			for _, capacityType := range it.Requirements.Get(karpv1.CapacityTypeLabelKey).Values() {
				// Reserved capacity types are constructed separately
				if capacityType == karpv1.CapacityTypeReserved {
					continue
				}
				isUnavailable := p.unavailableOfferings.IsUnavailable(ec2types.InstanceType(it.Name), zone, capacityType)
				var price float64
				var hasPrice bool
				switch capacityType {
				case karpv1.CapacityTypeOnDemand:
					price, hasPrice = p.pricingProvider.OnDemandPrice(ec2types.InstanceType(it.Name))
				case karpv1.CapacityTypeSpot:
					price, hasPrice = p.pricingProvider.SpotPrice(ec2types.InstanceType(it.Name), zone)
				default:
					panic(fmt.Sprintf("invalid capacity type %q in requirements for instance type %q", capacityType, it.Name))
				}
				offering := &cloudprovider.Offering{
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, capacityType),
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, zone),
						scheduling.NewRequirement(cloudprovider.ReservationIDLabel, corev1.NodeSelectorOpDoesNotExist),
						scheduling.NewRequirement(v1.LabelCapacityReservationType, corev1.NodeSelectorOpDoesNotExist),
					),
					Price:     price,
					Available: !isUnavailable && hasPrice && itZones.Has(zone),
				}
				if id, ok := subnetZonesToZoneIDs[zone]; ok {
					offering.Requirements.Add(scheduling.NewRequirement(v1.LabelTopologyZoneID, corev1.NodeSelectorOpIn, id))
				}
				cachedOfferings = append(cachedOfferings, offering)
			}
		}
		p.cache.SetDefault(p.cacheKeyFromInstanceType(it), cachedOfferings)
		offerings = append(offerings, cachedOfferings...)
	}
	if !options.FromContext(ctx).FeatureGates.ReservedCapacity {
		return offerings
	}

	capacityReservations := nodeClass.CapacityReservations()
	for i := range capacityReservations {
		if capacityReservations[i].InstanceType != it.Name {
			continue
		}
		reservation := &capacityReservations[i]
		price := 0.0
		if odPrice, ok := p.pricingProvider.OnDemandPrice(ec2types.InstanceType(it.Name)); ok {
			// Divide the on-demand price by a sufficiently large constant. This allows us to treat the reservation as "free",
			// while maintaining relative ordering for consolidation. If the pricing details are unavailable for whatever reason,
			// still succeed to create the offering and leave the price at zero. This will break consolidation, but will allow
			// users to utilize the instances they're already paying for.
			price = odPrice / 10_000_000.0
		}
		reservationCapacity := p.capacityReservationProvider.GetAvailableInstanceCount(reservation.ID)
		offering := &cloudprovider.Offering{
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeReserved),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, reservation.AvailabilityZone),
				scheduling.NewRequirement(cloudprovider.ReservationIDLabel, corev1.NodeSelectorOpIn, reservation.ID),
				scheduling.NewRequirement(v1.LabelCapacityReservationType, corev1.NodeSelectorOpIn, string(reservation.ReservationType)),
			),
			Price:               price,
			Available:           reservationCapacity != 0 && itZones.Has(reservation.AvailabilityZone),
			ReservationCapacity: reservationCapacity,
		}
		if id, ok := subnetZonesToZoneIDs[reservation.AvailabilityZone]; ok {
			offering.Requirements.Add(scheduling.NewRequirement(v1.LabelTopologyZoneID, corev1.NodeSelectorOpIn, id))
		}
		offerings = append(offerings, offering)
	}
	return offerings
}

func (p *DefaultProvider) cacheKeyFromInstanceType(it *cloudprovider.InstanceType) string {
	zonesHash, _ := hashstructure.Hash(
		it.Requirements.Get(corev1.LabelTopologyZone).Values(),
		hashstructure.FormatV2,
		&hashstructure.HashOptions{SlicesAsSets: true},
	)
	capacityTypesHash, _ := hashstructure.Hash(
		it.Requirements.Get(karpv1.CapacityTypeLabelKey).Values(),
		hashstructure.FormatV2,
		&hashstructure.HashOptions{SlicesAsSets: true},
	)
	return fmt.Sprintf(
		"%s-%016x-%016x-%d",
		it.Name,
		zonesHash,
		capacityTypesHash,
		p.unavailableOfferings.SeqNum,
	)
}
