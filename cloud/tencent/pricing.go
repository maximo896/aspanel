package tencent

import "sort"

func FilterAndSortOffers(offers []SpotOffer, maxPriceUSD float64) []SpotOffer {
	filtered := make([]SpotOffer, 0, len(offers))
	for _, offer := range offers {
		if offer.PriceUSD <= 0 {
			continue
		}
		if maxPriceUSD > 0 && offer.PriceUSD > maxPriceUSD {
			continue
		}
		filtered = append(filtered, offer)
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].PriceUSD < filtered[j].PriceUSD
	})
	return filtered
}

func MaxInstancesByHourlyBudget(offers []SpotOffer, hourlyBudgetUSD float64) int {
	if hourlyBudgetUSD <= 0 {
		return 0
	}
	total := 0.0
	count := 0
	for _, offer := range offers {
		if total+offer.PriceUSD > hourlyBudgetUSD {
			break
		}
		total += offer.PriceUSD
		count++
	}
	return count
}
