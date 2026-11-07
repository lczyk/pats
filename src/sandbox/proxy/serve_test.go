package proxy

import (
	"encoding/json"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// a run's metadata.json records the policy in the file's own snake_case.
func TestPolicyJSONNames(t *testing.T) {
	b, err := json.Marshal(Policy{Default: "allow", Methods: []string{"GET"}, AllowPrivate: true,
		Allow: []string{"a"}, Deny: []string{"b"}, DenyResponse: "404", Harness: []string{"h"}})
	require.NoError(t, err)
	assert.Equal(t, string(b), `{"default":"allow","methods":["GET"],"allow_private":true,"allow":["a"],"deny":["b"],"deny_response":"404","harness":["h"]}`)
}
