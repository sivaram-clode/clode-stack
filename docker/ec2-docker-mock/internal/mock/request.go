package mock

import (
	"fmt"
	"net/url"
	"strings"
)

// QueryRequest is an EC2 wire-protocol POST body parsed into a plain map plus
// the extracted top-level Action. Values reachable via indexed keys
// (`Foo.Bar.1.Baz=x`) are kept verbatim — callers use listValues / mapValues to
// walk them.
type QueryRequest struct {
	Action  string
	Version string
	values  url.Values
}

func parseQueryRequest(body []byte) (*QueryRequest, error) {
	v, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	action := v.Get("Action")
	if action == "" {
		return nil, fmt.Errorf("missing Action")
	}
	return &QueryRequest{Action: action, Version: v.Get("Version"), values: v}, nil
}

// get returns the first value for a key.
func (r *QueryRequest) get(key string) string { return r.values.Get(key) }

// listValues collects all values matching `prefix.N=…` where N is 1..M
// contiguous. EC2 uses this pattern for InstanceId.1, InstanceId.2 etc.
func (r *QueryRequest) listValues(prefix string) []string {
	out := []string{}
	for i := 1; ; i++ {
		v := r.values.Get(fmt.Sprintf("%s.%d", prefix, i))
		if v == "" {
			break
		}
		out = append(out, v)
	}
	return out
}

// tagSpecifications walks TagSpecification.N.Tag.M.{Key,Value} groups and
// flattens them into a single key→value map. brahmi tags instances with
// AGENT_ID, PROJECT_ID, SERVICE_SLUG, etc.; the resource-type dimension is
// ignored because docker labels are a flat namespace.
func (r *QueryRequest) tagSpecifications() map[string]string {
	tags := map[string]string{}
	for specIdx := 1; ; specIdx++ {
		anyThisSpec := false
		for tagIdx := 1; ; tagIdx++ {
			k := r.values.Get(fmt.Sprintf("TagSpecification.%d.Tag.%d.Key", specIdx, tagIdx))
			if k == "" {
				break
			}
			v := r.values.Get(fmt.Sprintf("TagSpecification.%d.Tag.%d.Value", specIdx, tagIdx))
			tags[k] = v
			anyThisSpec = true
		}
		if !anyThisSpec {
			break
		}
	}
	return tags
}

// describeFilters walks Filter.N.Name=… and Filter.N.Value.M=… (repeated) and
// returns them as a name→values map.
func (r *QueryRequest) describeFilters() map[string][]string {
	filters := map[string][]string{}
	for i := 1; ; i++ {
		name := r.values.Get(fmt.Sprintf("Filter.%d.Name", i))
		if name == "" {
			break
		}
		var vals []string
		for j := 1; ; j++ {
			v := r.values.Get(fmt.Sprintf("Filter.%d.Value.%d", i, j))
			if v == "" {
				break
			}
			vals = append(vals, v)
		}
		filters[name] = vals
	}
	return filters
}

// hibernateFlag reads the top-level Hibernate=true param used by StopInstances.
func (r *QueryRequest) hibernateFlag() bool {
	return strings.EqualFold(r.get("Hibernate"), "true")
}
