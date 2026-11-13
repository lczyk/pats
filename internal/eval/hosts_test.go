package eval

import (
	"bytes"
	"testing"

	"github.com/lczyk/assert"
)

// a build log's step and warning lines reach the run's output as they
// complete, however the writes split them; the log keeps every byte.
func TestStepLog(t *testing.T) {
	var all bytes.Buffer
	var steps, warns []string
	l := &stepLog{w: &all, step: func(m string) { steps = append(steps, m) }, warn: func(m string) { warns = append(warns, m) }}
	for _, chunk := range []string{"==> build", "ing x\nnoise\n==> warning: low", " space\n==> half"} {
		_, err := l.Write([]byte(chunk))
		assert.NoError(t, err)
	}
	assert.EqualArrays(t, steps, []string{"building x"})
	assert.EqualArrays(t, warns, []string{"low space"})
	assert.Equal(t, all.String(), "==> building x\nnoise\n==> warning: low space\n==> half")
}
