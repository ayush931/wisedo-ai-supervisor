package factorysim

import "testing"

func TestIdentitySeparatesSeedAndEntity(t *testing.T) {
	if ID(1, "line") == ID(2, "line") || ID(1, "line") == ID(1, "station") {
		t.Fatal("identity collision")
	}
}
