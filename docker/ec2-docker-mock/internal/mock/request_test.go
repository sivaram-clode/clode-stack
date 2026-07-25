package mock

import (
	"reflect"
	"testing"
)

func TestParseQueryRequest(t *testing.T) {
	req, err := parseQueryRequest([]byte("Action=RunInstances&Version=2016-11-15&ImageId=ami-123&MinCount=1&MaxCount=1"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Action != "RunInstances" {
		t.Errorf("Action=%q want RunInstances", req.Action)
	}
	if req.get("ImageId") != "ami-123" {
		t.Errorf("ImageId=%q want ami-123", req.get("ImageId"))
	}
}

func TestListValues(t *testing.T) {
	req, _ := parseQueryRequest([]byte("Action=X&InstanceId.1=i-a&InstanceId.2=i-b&InstanceId.3=i-c"))
	got := req.listValues("InstanceId")
	want := []string{"i-a", "i-b", "i-c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestTagSpecifications(t *testing.T) {
	body := "Action=RunInstances" +
		"&TagSpecification.1.ResourceType=instance" +
		"&TagSpecification.1.Tag.1.Key=Name" +
		"&TagSpecification.1.Tag.1.Value=agent-1" +
		"&TagSpecification.1.Tag.2.Key=ENV" +
		"&TagSpecification.1.Tag.2.Value=dev"
	req, _ := parseQueryRequest([]byte(body))
	tags := req.tagSpecifications()
	if tags["Name"] != "agent-1" || tags["ENV"] != "dev" {
		t.Errorf("bad tags: %v", tags)
	}
}

func TestDescribeFilters(t *testing.T) {
	body := "Action=DescribeInstances" +
		"&Filter.1.Name=tag%3Aenv&Filter.1.Value.1=dev" +
		"&Filter.2.Name=instance-state-name&Filter.2.Value.1=running&Filter.2.Value.2=pending"
	req, _ := parseQueryRequest([]byte(body))
	got := req.describeFilters()
	if !reflect.DeepEqual(got["tag:env"], []string{"dev"}) {
		t.Errorf("tag:env got %v", got["tag:env"])
	}
	if !reflect.DeepEqual(got["instance-state-name"], []string{"running", "pending"}) {
		t.Errorf("instance-state-name got %v", got["instance-state-name"])
	}
}

func TestHibernateFlag(t *testing.T) {
	req, _ := parseQueryRequest([]byte("Action=StopInstances&InstanceId.1=i-x&Hibernate=true"))
	if !req.hibernateFlag() {
		t.Error("hibernateFlag() = false, want true")
	}
	req2, _ := parseQueryRequest([]byte("Action=StopInstances&InstanceId.1=i-x"))
	if req2.hibernateFlag() {
		t.Error("hibernateFlag() = true when unset")
	}
}
