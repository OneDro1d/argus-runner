package main

import (
	"reflect"
	"testing"
)

// the ports come out of `docker compose config --format json`, never from a list here.
func TestComposePortsFromJSON_ReadsPublishedHostPortsOnly(t *testing.T) {
	cfg := `{"name":"argus-obs","services":{
	  "prometheus":{"ports":[{"mode":"ingress","host_ip":"127.0.0.1","target":9090,"published":"4124","protocol":"tcp"}]},
	  "grafana":{"ports":[{"mode":"ingress","target":3000,"published":"4123","protocol":"tcp"},
	                      {"target":3001,"published":"8000-8010"},{"target":3002}]},
	  "sidecar":{"ports":[{"target":80,"published":4123}]},
	  "none":{}}}`
	got, err := composePortsFromJSON([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{4123, 4124}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %v, want %v (a range and an unpublished target are not host ports; duplicates collapse)", got, want)
	}
}

func TestComposePortsFromJSON_GarbageIsAnErrorNotNoPorts(t *testing.T) {
	if got, err := composePortsFromJSON([]byte("not json")); err == nil {
		t.Fatalf("ports = %v, err nil — output that could not be read must not read as 'publishes nothing'", got)
	}
}
