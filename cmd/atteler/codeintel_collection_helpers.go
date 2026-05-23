package main

import "sort"

func filterCodeIntelSlice[T any](values []T, keep func(T) bool) []T {
	matches := make([]T, 0, len(values))

	for i := range values {
		if keep(values[i]) {
			matches = append(matches, values[i])
		}
	}

	return matches
}

func paginateCodeIntelSlice[T any](values []T, limit, offset int) []T {
	if limit <= 0 && offset <= 0 {
		return values
	}

	if offset >= len(values) {
		return nil
	}

	if offset < 0 {
		offset = 0
	}

	end := len(values)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}

	return values[offset:end]
}

func sortCodeIntelSlice[T any](values []T, less func(T, T) bool) {
	sort.Slice(values, func(i, j int) bool {
		return less(values[i], values[j])
	})
}

func sortCodeIntelStringsAsc(values []string) {
	sort.Strings(values)
}

func sortCodeIntelByNameAsc[T any](values []T, name func(T) string) {
	sortCodeIntelSlice(values, func(left, right T) bool {
		return name(left) < name(right)
	})
}

func sortCodeIntelByNameAscCountAsc[T any](values []T, name func(T) string, count func(T) int) {
	sortCodeIntelSlice(values, func(left, right T) bool {
		if name(left) != name(right) {
			return name(left) < name(right)
		}

		return count(left) < count(right)
	})
}

func sortCodeIntelByNamesAsc[T any](values []T, primary, secondary func(T) string) {
	sortCodeIntelSlice(values, func(left, right T) bool {
		if primary(left) != primary(right) {
			return primary(left) < primary(right)
		}

		return secondary(left) < secondary(right)
	})
}

func sortCodeIntelByNamesAscLineAsc[T any](values []T, primary, secondary func(T) string, line func(T) int) {
	sortCodeIntelSlice(values, func(left, right T) bool {
		if primary(left) != primary(right) {
			return primary(left) < primary(right)
		}

		if secondary(left) != secondary(right) {
			return secondary(left) < secondary(right)
		}

		return line(left) < line(right)
	})
}

func sortCodeIntelByCountDescNameAsc[T any](values []T, count func(T) int, name func(T) string) {
	sortCodeIntelSlice(values, func(left, right T) bool {
		if count(left) != count(right) {
			return count(left) > count(right)
		}

		return name(left) < name(right)
	})
}

func sortCodeIntelByCountsDescNameAsc[T any](values []T, primary, secondary func(T) int, name func(T) string) {
	sortCodeIntelSlice(values, func(left, right T) bool {
		if primary(left) != primary(right) {
			return primary(left) > primary(right)
		}

		if secondary(left) != secondary(right) {
			return secondary(left) > secondary(right)
		}

		return name(left) < name(right)
	})
}
