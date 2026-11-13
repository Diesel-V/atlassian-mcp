package scope

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"atlassian-mcp/internal/atlassian"
)

func TestZeroValueAllowsNothing(t *testing.T) {
	var s Set
	if s.Allows("ANY") || s.Unrestricted() {
		t.Fatal("zero Set must deny everything")
	}
	if _, err := s.Restrict([]string{"ANY"}, "spaces"); !errors.Is(err, atlassian.ErrInvalidParams) {
		t.Fatalf("restrict on empty set: %v", err)
	}
	if keys, err := s.Restrict(nil, "spaces"); err != nil || keys != nil {
		t.Fatalf("empty set without request: %v %v", keys, err)
	}
}

func TestParseAndAllows(t *testing.T) {
	if !Parse([]string{"*"}).Unrestricted() || !Parse([]string{" * "}).Allows("X") {
		t.Fatal("* must allow all")
	}
	s := Parse([]string{"TECHOPS", "dba", " ", "~i.ivanov", "techops"})
	if got := s.Keys(); !slices.Equal(got, []string{"TECHOPS", "dba", "~i.ivanov"}) {
		t.Fatalf("keys = %v", got)
	}
	for _, k := range []string{"TECHOPS", "techops", "DBA", " ~I.IVANOV "} {
		if !s.Allows(k) {
			t.Errorf("%q must be allowed", k)
		}
	}
	for _, k := range []string{"", "HR", "TECH", "*"} {
		if s.Allows(k) {
			t.Errorf("%q must not be allowed", k)
		}
	}
	if Parse([]string{"*", "HR"}).Unrestricted() {
		t.Fatal("* mixed with keys must not mean all")
	}
}

func TestRestrict(t *testing.T) {
	s := Of("OD", "DEVOPS")
	keys, err := s.Restrict(nil, "projects")
	if err != nil || !slices.Equal(keys, []string{"OD", "DEVOPS"}) {
		t.Fatalf("default = %v %v", keys, err)
	}
	keys, err = s.Restrict([]string{"od"}, "projects")
	if err != nil || !slices.Equal(keys, []string{"od"}) {
		t.Fatalf("subset = %v %v", keys, err)
	}
	_, err = s.Restrict([]string{"OD", "HR"}, "projects")
	if !errors.Is(err, atlassian.ErrInvalidParams) || !strings.Contains(err.Error(), `"HR"`) || !strings.Contains(err.Error(), "OD, DEVOPS") {
		t.Fatalf("outside = %v", err)
	}
	if keys, err := All().Restrict(nil, "projects"); err != nil || keys != nil {
		t.Fatalf("unrestricted default = %v %v", keys, err)
	}
	if keys, _ := All().Restrict([]string{"HR"}, "projects"); !slices.Equal(keys, []string{"HR"}) {
		t.Fatalf("unrestricted passthrough = %v", keys)
	}
}

func TestDescribe(t *testing.T) {
	if All().Describe("spaces") != "" {
		t.Fatal("unrestricted must not describe")
	}
	if d := Of("A", "B").Describe("spaces"); d != " Only these spaces are available: A, B." {
		t.Fatalf("describe = %q", d)
	}
}
