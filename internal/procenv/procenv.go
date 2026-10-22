// Package procenv is the process environment as a map, and back as a list
// sorted by name. The session builds the environment it hands the harness
// with it, and the glab issuer strips its own token from glab's.
package procenv

import (
	"sort"
	"strings"
)

// Map splits name=value entries into a map; the last entry of a name wins, as
// in an environment where a later variable replaces an earlier one.
func Map(values []string) map[string]string {
	result := map[string]string{}
	for _, value := range values {
		key, v, ok := strings.Cut(value, "=")
		if ok {
			result[key] = v
		}
	}
	return result
}

// List writes a map as name=value entries, sorted by name, so that what geas
// hands a child process does not depend on map iteration order.
func List(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}
