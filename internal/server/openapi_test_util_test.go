package server

import (
	"encoding/json"
	"regexp"
	"testing"
)

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var refRe = regexp.MustCompile(`"\$ref":"([^"]+)"`)

func findRefs(s string) []string {
	var out []string
	for _, m := range refRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}
