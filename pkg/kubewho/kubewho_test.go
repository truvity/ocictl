package kubewho

import (
	"strings"
	"testing"
)

func TestParseWhoAmI(t *testing.T) {
	data := []byte(`{"kind":"SelfSubjectReview","status":{"userInfo":{
		"username":"o.tsarev@truvity.com",
		"groups":["cluster-devel:cluster:viewer","emp:otsar","system:authenticated"]}}}`)

	u, err := parseWhoAmI(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if u.Username != "o.tsarev@truvity.com" || len(u.Groups) != 3 {
		t.Errorf("unexpected user: %+v", u)
	}
}

func TestParseWhoAmIUnauthenticated(t *testing.T) {
	if _, err := parseWhoAmI([]byte(`{"status":{"userInfo":{}}}`)); err == nil {
		t.Error("empty username must error")
	}
}

func TestGroupPrefixedExactlyOne(t *testing.T) {
	u := User{Username: "x@example.com", Groups: []string{"emp:otsar", "cluster-devel:cluster:viewer"}}

	slug, err := GroupPrefixed(u, "emp:")
	if err != nil || slug != "otsar" {
		t.Errorf("want otsar, got %q err %v", slug, err)
	}
}

func TestGroupPrefixedZeroIsError(t *testing.T) {
	u := User{Username: "robot@example.com", Groups: []string{"system:authenticated"}}

	_, err := GroupPrefixed(u, "emp:")
	if err == nil || !strings.Contains(err.Error(), "no \"emp:\" group") {
		t.Errorf("zero matches must error with guidance, got %v", err)
	}
}

func TestGroupPrefixedMultipleIsError(t *testing.T) {
	u := User{Username: "x@example.com", Groups: []string{"emp:a", "emp:b"}}

	if _, err := GroupPrefixed(u, "emp:"); err == nil {
		t.Error("multiple matches must error, never guess")
	}
}
