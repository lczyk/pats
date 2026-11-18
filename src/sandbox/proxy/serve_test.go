package proxy

import (
	"encoding/json"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"gopkg.in/yaml.v3"
)

// a run's metadata.json records the policy in the file's own snake_case.
func TestPolicyJSONNames(t *testing.T) {
	b, err := json.Marshal(Policy{Default: "allow", Methods: []string{"GET"}, AllowPrivate: true,
		Allow: []string{"a"}, Deny: []string{"b"}, DenyResponse: "404", Harness: []string{"h"}})
	require.NoError(t, err)
	assert.Equal(t, string(b), `{"default":"allow","methods":["GET"],"allow_private":true,"allow":["a"],"deny":["b"],"deny_response":"404","harness":["h"]}`)
}

// an empty deny-message is not an absent one, in the json a run records and
// in the yaml a proxy is handed.
func TestDenyMessageSurvivesEncoding(t *testing.T) {
	for _, msg := range []string{"", "not here\n"} {
		in := Policy{DenyMessage: &msg}
		b, err := json.Marshal(in)
		require.NoError(t, err)
		var fromJSON Policy
		require.NoError(t, json.Unmarshal(b, &fromJSON))
		require.That(t, fromJSON.DenyMessage != nil, "json dropped %q", msg)
		assert.Equal(t, *fromJSON.DenyMessage, msg)

		y, err := yaml.Marshal(in)
		require.NoError(t, err)
		fromYAML, err := ParsePolicy(y)
		require.NoError(t, err)
		assert.Equal(t, *fromYAML.DenyMessage, msg)
	}
	b, err := json.Marshal(Policy{DenyResponse: "404"}.Resolved())
	require.NoError(t, err)
	assert.Equal(t, string(b), `{"deny_response":"404"}`)
}
