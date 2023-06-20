package shared

func Deduplicate[T any](locations []T, keyFn func(T) string) []T {
	seen := map[string]struct{}{}

	filtered := locations[:0]
	for _, l := range locations {
		k := keyFn(l)
		if _, ok := seen[k]; ok {
			continue
		}

		seen[k] = struct{}{}
		filtered = append(filtered, l)
	}

	return filtered
}
