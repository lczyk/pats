package main

import (
	"testing"

	"github.com/lczyk/assert"
)

func TestSplitList(t *testing.T) {
	got := splitList(" a, ,b,,c ")
	want := []string{"a", "b", "c"}
	assert.EqualArrays(t, got, want)
}
