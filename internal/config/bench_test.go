package config

import (
	"os"
	"testing"

	"github.com/lczyk/assert/require"
)

// what every pats command pays before it does anything: the example config
// parsed and validated, its file already read.
func BenchmarkParseConfig(b *testing.B) {
	data, err := os.ReadFile("../../pats.example.yaml")
	require.NoError(b, err)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		c, err := ParseIn("../..", data)
		if err != nil {
			b.Fatal(err)
		}
		if err := c.Validate(); err != nil {
			b.Fatal(err)
		}
	}
}
