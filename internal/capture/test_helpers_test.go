package capture

import (
	"encoding/json"
	"os"
	"testing"
)

func readFixtureFile(t *testing.T, path string) Fixture {
	t.Helper()
	fixtureBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture Fixture
	if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}
