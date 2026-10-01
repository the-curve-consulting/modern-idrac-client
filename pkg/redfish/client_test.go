package redfish

import (
	"encoding/json"
	"testing"
)

func TestObjectLookup(t *testing.T) {
	var o Object
	json.Unmarshal([]byte(`{"@odata.type":"#ServiceRoot.v1_3_0.ServiceRoot","Status":{"Health":"OK"},"ProcessorSummary":{"Count":2},"Members":[{"@odata.id":"/a"},{"@odata.id":"/b"}]}`), &o)
	if o.Str("@odata.type") != "#ServiceRoot.v1_3_0.ServiceRoot" {
		t.Fatalf("dotted key: %q", o.Str("@odata.type"))
	}
	if o.Str("Status.Health") != "OK" {
		t.Fatal("nested")
	}
	if o.Str("ProcessorSummary.Count") != "2" {
		t.Fatalf("number: %q", o.Str("ProcessorSummary.Count"))
	}
	if l := o.List("Members"); len(l) != 2 || l[1].ID() != "/b" {
		t.Fatalf("list: %v", l)
	}
}

func TestParseError(t *testing.T) {
	body := []byte(`{"error":{"code":"Base.1.0.GeneralError","message":"A general error has occurred.","@Message.ExtendedInfo":[{"MessageId":"IDRAC.1.6.SYS402","Message":"Unable to perform the action.","Resolution":"Retry later."}]}}`)
	err := parseError("POST", "/x", 400, body)
	e, ok := err.(*Error)
	if !ok || e.Status != 400 || len(e.Messages) != 1 || e.Messages[0] != "IDRAC.1.6.SYS402: Unable to perform the action. (Retry later.)" {
		t.Fatalf("got %v", err)
	}
}
