package memory

import "testing"

func TestCapabilityReadOnly(t *testing.T) {
	for _, c := range Capabilities(nil) {
		spec := c.Spec()
		if want := spec.Name == "memory_search"; spec.ReadOnly != want {
			t.Errorf("%s ReadOnly = %v, want %v", spec.Name, spec.ReadOnly, want)
		}
	}
}
