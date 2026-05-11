package tencent

import "strings"

func IsMainlandRegion(region string) bool {
	r := strings.ToLower(strings.TrimSpace(region))
	return strings.HasPrefix(r, "ap-beijing") ||
		strings.HasPrefix(r, "ap-shanghai") ||
		strings.HasPrefix(r, "ap-guangzhou") ||
		strings.HasPrefix(r, "ap-nanjing") ||
		strings.HasPrefix(r, "ap-chengdu") ||
		strings.HasPrefix(r, "ap-chongqing") ||
		strings.HasPrefix(r, "ap-wuhan")
}

func IsHKMOTWRegion(region string) bool {
	r := strings.ToLower(strings.TrimSpace(region))
	return strings.HasPrefix(r, "ap-hongkong") ||
		strings.HasPrefix(r, "ap-taipei") ||
		strings.HasPrefix(r, "ap-macau")
}

func FilterNonMainland(regions []string) []string {
	out := make([]string, 0, len(regions))
	for _, region := range regions {
		if !IsMainlandRegion(region) && !IsHKMOTWRegion(region) {
			out = append(out, region)
		}
	}
	return out
}
