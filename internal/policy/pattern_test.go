package policy

import "testing"

func TestDoubleStarSubdomainWildcardIsTheSingleStarOne(t *testing.T) {
	p, err := ParsePattern("**.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if p.String() != "*.example.com" {
		t.Errorf("String() = %q", p.String())
	}
}
