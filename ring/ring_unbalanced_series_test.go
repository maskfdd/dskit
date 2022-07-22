package ring

import (
	"fmt"
	"math"
	"regexp"
	"testing"
	"time"
)

func TestInvestigateUnbalanceSeriesPerIngester(t *testing.T) {
	now := time.Now().Unix()
	desc := unbalancedSeriesRingDesc

	// Update the ring to ensure all instances are ACTIVE and healthy.
	for id, instance := range desc.Ingesters {
		instance.State = ACTIVE
		instance.Timestamp = now
		desc.Ingesters[id] = instance
	}

	// Create a ring with the instances.
	ring := Ring{
		cfg: Config{
			HeartbeatTimeout:     time.Hour,
			ReplicationFactor:    3,
			ZoneAwarenessEnabled: true,
		},
		ringDesc:            desc,
		ringTokens:          desc.GetTokens(),
		ringTokensByZone:    desc.getTokensByZone(),
		ringInstanceByToken: desc.getTokensInfo(),
		ringZones:           getZones(desc.getTokensByZone()),
		strategy:            NewDefaultReplicationStrategy(),
	}

	// Compute statistics about % of owned tokens GLOBALLY.
	minOwnedPercentage, maxOwnedPercentage, maxVariance := computeMinAndMaxTokensOwnership(desc)
	fmt.Println(fmt.Sprintf("Global ownership: min=%.2f%% max=%.2f%% max variance=%.2f%%", minOwnedPercentage, maxOwnedPercentage, maxVariance))

	// Compute statistics about % of owned tokens PER-ZONE.
	// To compute it we need to only take in account ingesters in that zone.
	for _, zone := range ring.ringZones {
		// Build a ring description including only instances from the given zone.
		zoneDesc := &Desc{Ingesters: map[string]InstanceDesc{}}
		for id, instance := range desc.Ingesters {
			if instance.Zone == zone {
				zoneDesc.Ingesters[id] = instance
			}
		}

		minOwnedPercentage, maxOwnedPercentage, maxVariance := computeMinAndMaxTokensOwnership(zoneDesc)
		fmt.Println(fmt.Sprintf("Per-zone ownership: zone=%s min=%.2f%% max=%.2f%% max variance=%.2f%%", zone, minOwnedPercentage, maxOwnedPercentage, maxVariance))
	}

	// Compute statistics to find out whether the ingesters with less series are the ones owning less tokens.
	// Since we use zone-aware replication, we need to look at the per-zone ownership %.
	perZoneTokensOwnership := computePerZoneTokensOwnership(desc)
	perZoneSeriesOwnership := computePerZoneSeriesOwnership(inmemorySeriesPerIngester)
	seriesVsTokensCorrelationDistribution := make([]int, 10)
	seriesVsTokensCorrelationOutliers := map[string]int{}
	seriesVsTokensCorrelationThreshold := 80

	for zone, perIngesterTokensOwnership := range perZoneTokensOwnership {
		for ingesterID, tokensOwnership := range perIngesterTokensOwnership {
			seriesOwnership := perZoneSeriesOwnership[zone][ingesterID]

			// Compute a correlation score between [0, 100]. The higher the value, the higher the correlation between
			// the number of owned series and owned tokens.
			// This is a percentage: 100% means an ingester owns a number of series equal to the number of owned tokens.
			// 50% means an ingester owns either half or the double of series compared to the number of owned tokens.
			correlation := 100 - int(math.Round((math.Abs(seriesOwnership-tokensOwnership)/seriesOwnership)*100))

			// Increment the counter in the expect distribution bucket.
			if correlation < 100 {
				seriesVsTokensCorrelationDistribution[correlation/10]++
			} else {
				// Just to cover the case the value is 100.
				seriesVsTokensCorrelationDistribution[9]++
			}

			if correlation < seriesVsTokensCorrelationThreshold {
				seriesVsTokensCorrelationOutliers[ingesterID] = correlation
			}

			// fmt.Println(fmt.Sprintf("%s owns %.2f%% tokens and %.2f%% series, correlation: %d", ingesterID, tokensOwnership, seriesOwnership, correlation))
		}
	}

	fmt.Println("")
	fmt.Println("------------------------------------------------------")
	fmt.Println("")
	fmt.Println("Correlation between number of tokens owned and in-memory series")
	fmt.Println("This is a percentage: 100% means an ingester owns a number of series equal to the number of owned tokens.")
	fmt.Println("50% means an ingester owns either half or the double of series compared to the number of owned tokens.")
	fmt.Println("")
	for idx, numIngesters := range seriesVsTokensCorrelationDistribution {
		bucketStart := idx * 10
		bucketEnd := bucketStart + 10
		fmt.Println(fmt.Sprintf("[%3d, %3d] Number ingesters: %d", bucketStart, bucketEnd, numIngesters))
	}

	if len(seriesVsTokensCorrelationOutliers) > 0 {
		fmt.Println("")
		fmt.Println(fmt.Sprintf("Outliers (correlation < %d):", seriesVsTokensCorrelationThreshold))

		for ingesterID, correlation := range seriesVsTokensCorrelationOutliers {
			fmt.Println(fmt.Sprintf("- %s \twith correlation %d (number of series: %.3fM)", ingesterID, correlation, float64(inmemorySeriesPerIngester[ingesterID])/1000000))
		}
	}

}

// TODO test me
func computePerZoneTokensOwnership(desc *Desc) map[string]map[string]float64 {
	out := map[string]map[string]float64{}

	for zone, _ := range desc.getTokensByZone() {
		// Build a ring description including only instances from the given zone.
		zoneDesc := &Desc{Ingesters: map[string]InstanceDesc{}}
		for id, instance := range desc.Ingesters {
			if instance.Zone == zone {
				zoneDesc.Ingesters[id] = instance
			}
		}

		// Compute the per-ingester tokens ownership %.
		out[zone] = map[string]float64{}
		for id, numTokens := range zoneDesc.countTokens() {
			out[zone][id] = (float64(numTokens) / float64(math.MaxUint32)) * 100
		}
	}

	return out
}

// TODO test me
func computePerZoneSeriesOwnership(seriesPerIngester map[string]int) map[string]map[string]float64 {
	// Group ingesters by zone.
	zones := map[string]map[string]int{}
	for ingesterID, numSeries := range seriesPerIngester {
		zone := getZoneFromIngesterID(ingesterID)
		if _, ok := zones[zone]; !ok {
			zones[zone] = map[string]int{}
		}

		zones[zone][ingesterID] = numSeries
	}

	// Compute the per-zone ownership %.
	out := map[string]map[string]float64{}
	for zone, ingesters := range zones {
		// Count the total number of series in the zone.
		totalSeries := 0
		for _, numSeries := range ingesters {
			totalSeries += numSeries
		}

		// Compute the ownership %.
		out[zone] = map[string]float64{}
		for ingesterID, numSeries := range ingesters {
			out[zone][ingesterID] = (float64(numSeries) / float64(totalSeries)) * 100
		}
	}

	return out
}

// TODO test me
func computeMinAndMaxTokensOwnership(desc *Desc) (float64, float64, float64) {
	minOwnedPercentage := math.MaxFloat64
	maxOwnedPercentage := float64(0)

	for _, numTokens := range desc.countTokens() {
		ownedPercentage := (float64(numTokens) / float64(math.MaxUint32)) * 100
		if ownedPercentage < minOwnedPercentage {
			minOwnedPercentage = ownedPercentage
		}
		if ownedPercentage > maxOwnedPercentage {
			maxOwnedPercentage = ownedPercentage
		}
	}

	maxVariance := ((maxOwnedPercentage - minOwnedPercentage) / maxOwnedPercentage) * 100

	return minOwnedPercentage, maxOwnedPercentage, maxVariance
}

var ingesterIDRegex = regexp.MustCompile("^ingester-(zone-[a-z]{1})-\\d+$")

// TODO test me
func getZoneFromIngesterID(id string) string {
	parts := ingesterIDRegex.FindStringSubmatch(id)
	if len(parts) != 2 {
		panic(fmt.Sprintf("unable to extract zone ID from %q", id))
	}

	return parts[1]
}
