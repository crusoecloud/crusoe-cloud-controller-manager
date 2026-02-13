package node_test

import (
	"testing"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/node"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
)

func TestParseTaintsFromLabels(t *testing.T) { //nolint:funlen // table-driven test
	t.Parallel()

	tests := []struct {
		name       string
		nodeLabels map[string]string
		expected   []v1.Taint
	}{
		{
			name:       "empty labels",
			nodeLabels: map[string]string{},
			expected:   nil,
		},
		{
			name: "no taint labels",
			nodeLabels: map[string]string{
				"some-label": "some-value",
			},
			expected: nil,
		},
		{
			name: "simple taint with NoSchedule effect",
			nodeLabels: map[string]string{
				"crusoe.aai/taint.gpu-workload": "NoSchedule",
			},
			expected: []v1.Taint{
				{
					Key:    "crusoe.aai/gpu-workload",
					Value:  "",
					Effect: v1.TaintEffectNoSchedule,
				},
			},
		},
		{
			name: "simple taint with PreferNoSchedule effect",
			nodeLabels: map[string]string{
				"crusoe.aai/taint.dedicated": "PreferNoSchedule",
			},
			expected: []v1.Taint{
				{
					Key:    "crusoe.aai/dedicated",
					Value:  "",
					Effect: v1.TaintEffectPreferNoSchedule,
				},
			},
		},
		{
			name: "simple taint with NoExecute effect",
			nodeLabels: map[string]string{
				"crusoe.aai/taint.maintenance": "NoExecute",
			},
			expected: []v1.Taint{
				{
					Key:    "crusoe.aai/maintenance",
					Value:  "",
					Effect: v1.TaintEffectNoExecute,
				},
			},
		},
		{
			name: "taint with value:effect format",
			nodeLabels: map[string]string{
				"crusoe.aai/taint.gpu-workload": "true:NoSchedule",
			},
			expected: []v1.Taint{
				{
					Key:    "crusoe.aai/gpu-workload",
					Value:  "true",
					Effect: v1.TaintEffectNoSchedule,
				},
			},
		},
		{
			name: "multiple taints",
			nodeLabels: map[string]string{
				"crusoe.aai/taint.gpu-workload": "NoSchedule",
				"crusoe.aai/taint.dedicated":    "team-a:PreferNoSchedule",
				"other-label":                   "other-value",
			},
			expected: []v1.Taint{
				{
					Key:    "crusoe.aai/gpu-workload",
					Value:  "",
					Effect: v1.TaintEffectNoSchedule,
				},
				{
					Key:    "crusoe.aai/dedicated",
					Value:  "team-a",
					Effect: v1.TaintEffectPreferNoSchedule,
				},
			},
		},
		{
			name: "invalid effect is skipped",
			nodeLabels: map[string]string{
				"crusoe.aai/taint.invalid": "InvalidEffect",
			},
			expected: nil,
		},
		{
			name: "empty taint key is skipped",
			nodeLabels: map[string]string{
				"crusoe.aai/taint.": "NoSchedule",
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := node.ParseTaintsFromLabels(tt.nodeLabels)

			if tt.expected == nil {
				assert.Nil(t, result)

				return
			}

			assert.Len(t, result, len(tt.expected))

			// Create a map for easier comparison since order is not guaranteed
			resultMap := make(map[string]v1.Taint)
			for _, taint := range result {
				resultMap[taint.Key] = taint
			}

			for _, expected := range tt.expected {
				actual, exists := resultMap[expected.Key]
				assert.True(t, exists, "expected taint key %s not found", expected.Key)
				assert.Equal(t, expected.Value, actual.Value)
				assert.Equal(t, expected.Effect, actual.Effect)
			}
		})
	}
}

func TestParseTaintEffect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected v1.TaintEffect
	}{
		{"NoSchedule", v1.TaintEffectNoSchedule},
		{"PreferNoSchedule", v1.TaintEffectPreferNoSchedule},
		{"NoExecute", v1.TaintEffectNoExecute},
		{"invalid", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()

			result := node.ParseTaintEffect(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetManagedTaints(t *testing.T) { //nolint:funlen // table-driven test
	t.Parallel()

	tests := []struct {
		name     string
		taints   []v1.Taint
		expected []v1.Taint
	}{
		{
			name:     "empty taints",
			taints:   []v1.Taint{},
			expected: nil,
		},
		{
			name: "no managed taints",
			taints: []v1.Taint{
				{Key: "node.kubernetes.io/not-ready", Effect: v1.TaintEffectNoSchedule},
			},
			expected: nil,
		},
		{
			name: "only managed taints",
			taints: []v1.Taint{
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoSchedule},
				{Key: "crusoe.aai/dedicated", Value: "team-a", Effect: v1.TaintEffectPreferNoSchedule},
			},
			expected: []v1.Taint{
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoSchedule},
				{Key: "crusoe.aai/dedicated", Value: "team-a", Effect: v1.TaintEffectPreferNoSchedule},
			},
		},
		{
			name: "mixed taints",
			taints: []v1.Taint{
				{Key: "node.kubernetes.io/not-ready", Effect: v1.TaintEffectNoSchedule},
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoSchedule},
				{Key: "other-taint", Effect: v1.TaintEffectNoExecute},
			},
			expected: []v1.Taint{
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoSchedule},
			},
		},
		{
			name: "excludes shutdown taint",
			taints: []v1.Taint{
				{Key: "node.cloudprovider.kubernetes.io/shutdown", Effect: v1.TaintEffectNoSchedule},
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoSchedule},
			},
			expected: []v1.Taint{
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoSchedule},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := node.GetManagedTaints(tt.taints)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDiffTaints(t *testing.T) { //nolint:funlen // table-driven test
	t.Parallel()

	tests := []struct {
		name           string
		current        []v1.Taint
		desired        []v1.Taint
		expectedAdd    []v1.Taint
		expectedRemove []v1.Taint
	}{
		{
			name:           "both empty",
			current:        []v1.Taint{},
			desired:        []v1.Taint{},
			expectedAdd:    nil,
			expectedRemove: nil,
		},
		{
			name:    "add new taint",
			current: []v1.Taint{},
			desired: []v1.Taint{
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoSchedule},
			},
			expectedAdd: []v1.Taint{
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoSchedule},
			},
			expectedRemove: nil,
		},
		{
			name: "remove taint",
			current: []v1.Taint{
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoSchedule},
			},
			desired:     []v1.Taint{},
			expectedAdd: nil,
			expectedRemove: []v1.Taint{
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoSchedule},
			},
		},
		{
			name: "no changes needed",
			current: []v1.Taint{
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoSchedule},
			},
			desired: []v1.Taint{
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoSchedule},
			},
			expectedAdd:    nil,
			expectedRemove: nil,
		},
		{
			name: "update taint effect",
			current: []v1.Taint{
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoSchedule},
			},
			desired: []v1.Taint{
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoExecute},
			},
			expectedAdd: []v1.Taint{
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoExecute},
			},
			expectedRemove: []v1.Taint{
				{Key: "crusoe.aai/gpu-workload", Effect: v1.TaintEffectNoSchedule},
			},
		},
		{
			name: "update taint value",
			current: []v1.Taint{
				{Key: "crusoe.aai/dedicated", Value: "team-a", Effect: v1.TaintEffectNoSchedule},
			},
			desired: []v1.Taint{
				{Key: "crusoe.aai/dedicated", Value: "team-b", Effect: v1.TaintEffectNoSchedule},
			},
			expectedAdd: []v1.Taint{
				{Key: "crusoe.aai/dedicated", Value: "team-b", Effect: v1.TaintEffectNoSchedule},
			},
			expectedRemove: []v1.Taint{
				{Key: "crusoe.aai/dedicated", Value: "team-a", Effect: v1.TaintEffectNoSchedule},
			},
		},
		{
			name: "complex diff",
			current: []v1.Taint{
				{Key: "crusoe.aai/to-keep", Effect: v1.TaintEffectNoSchedule},
				{Key: "crusoe.aai/to-remove", Effect: v1.TaintEffectNoSchedule},
				{Key: "crusoe.aai/to-update", Value: "old", Effect: v1.TaintEffectNoSchedule},
			},
			desired: []v1.Taint{
				{Key: "crusoe.aai/to-keep", Effect: v1.TaintEffectNoSchedule},
				{Key: "crusoe.aai/to-add", Effect: v1.TaintEffectNoExecute},
				{Key: "crusoe.aai/to-update", Value: "new", Effect: v1.TaintEffectNoSchedule},
			},
			expectedAdd: []v1.Taint{
				{Key: "crusoe.aai/to-add", Effect: v1.TaintEffectNoExecute},
				{Key: "crusoe.aai/to-update", Value: "new", Effect: v1.TaintEffectNoSchedule},
			},
			expectedRemove: []v1.Taint{
				{Key: "crusoe.aai/to-remove", Effect: v1.TaintEffectNoSchedule},
				{Key: "crusoe.aai/to-update", Value: "old", Effect: v1.TaintEffectNoSchedule},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			toAdd, toRemove := node.DiffTaints(tt.current, tt.desired)

			// Convert to maps for easier comparison (order not guaranteed)
			addMap := make(map[string]v1.Taint)
			for _, t := range toAdd {
				addMap[t.Key] = t
			}
			removeMap := make(map[string]v1.Taint)
			for _, t := range toRemove {
				removeMap[t.Key] = t
			}

			expectedAddMap := make(map[string]v1.Taint)
			for _, t := range tt.expectedAdd {
				expectedAddMap[t.Key] = t
			}
			expectedRemoveMap := make(map[string]v1.Taint)
			for _, t := range tt.expectedRemove {
				expectedRemoveMap[t.Key] = t
			}

			assert.Equal(t, len(tt.expectedAdd), len(toAdd), "toAdd length mismatch")
			assert.Equal(t, len(tt.expectedRemove), len(toRemove), "toRemove length mismatch")

			for key, expected := range expectedAddMap {
				actual, exists := addMap[key]
				assert.True(t, exists, "expected add key %s not found", key)
				assert.Equal(t, expected.Value, actual.Value)
				assert.Equal(t, expected.Effect, actual.Effect)
			}

			for key, expected := range expectedRemoveMap {
				actual, exists := removeMap[key]
				assert.True(t, exists, "expected remove key %s not found", key)
				assert.Equal(t, expected.Value, actual.Value)
				assert.Equal(t, expected.Effect, actual.Effect)
			}
		})
	}
}
