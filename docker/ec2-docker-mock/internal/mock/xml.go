package mock

import (
	"encoding/xml"
	"log"
	"net/http"
)

// ec2Namespace matches what the SDK expects. aws-sdk-go-v2 is tolerant of
// version bumps so a slightly older date is fine.
const ec2Namespace = "http://ec2.amazonaws.com/doc/2016-11-15/"

// InstanceState maps to <instanceState><code/><name/></instanceState>.
type InstanceState struct {
	Code int    `xml:"code"`
	Name string `xml:"name"`
}

// EC2 canonical state codes.
const (
	stateCodePending      = 0
	stateCodeRunning      = 16
	stateCodeShuttingDown = 32
	stateCodeTerminated   = 48
	stateCodeStopping     = 64
	stateCodeStopped      = 80
)

func stateFromCode(code int) InstanceState {
	names := map[int]string{
		stateCodePending:      "pending",
		stateCodeRunning:      "running",
		stateCodeShuttingDown: "shutting-down",
		stateCodeTerminated:   "terminated",
		stateCodeStopping:     "stopping",
		stateCodeStopped:      "stopped",
	}
	return InstanceState{Code: code, Name: names[code]}
}

// Tag is emitted inside <tagSet><item/></tagSet>.
type Tag struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

// Instance is one row inside <instancesSet>. Only the fields brahmi actually
// reads are populated — the SDK ignores missing optional fields silently.
type Instance struct {
	InstanceID        string        `xml:"instanceId"`
	ImageID           string        `xml:"imageId"`
	State             InstanceState `xml:"instanceState"`
	PrivateDNSName    string        `xml:"privateDnsName,omitempty"`
	PublicDNSName     string        `xml:"dnsName,omitempty"`
	StateReason       *StateReason  `xml:"stateReason,omitempty"`
	AmiLaunchIndex    int           `xml:"amiLaunchIndex"`
	InstanceType      string        `xml:"instanceType"`
	LaunchTime        string        `xml:"launchTime"`
	Placement         Placement     `xml:"placement"`
	PrivateIPAddress  string        `xml:"privateIpAddress,omitempty"`
	PublicIPAddress   string        `xml:"ipAddress,omitempty"`
	RootDeviceType    string        `xml:"rootDeviceType"`
	RootDeviceName    string        `xml:"rootDeviceName"`
	VirtualizationTyp string        `xml:"virtualizationType"`
	Architecture      string        `xml:"architecture"`
	Hypervisor        string        `xml:"hypervisor"`
	InstanceLifecycle string        `xml:"instanceLifecycle,omitempty"`
	Tags              []Tag         `xml:"tagSet>item"`
	Hibernated        bool          `xml:"-"` // internal: not part of AWS wire format
}

// StateReason is populated for stopped/terminated instances so callers that
// distinguish spot-reclaim vs user-initiated shutdown see something meaningful.
type StateReason struct {
	Code    string `xml:"code"`
	Message string `xml:"message"`
}

// Placement carries the availability zone (returned as region+a).
type Placement struct {
	AvailabilityZone string `xml:"availabilityZone"`
}

// Reservation wraps one or more instances launched together.
type Reservation struct {
	ReservationID string     `xml:"reservationId"`
	OwnerID       string     `xml:"ownerId"`
	Instances     []Instance `xml:"instancesSet>item"`
}

// InstanceStateChange is used in Stop/Start/Terminate responses.
type InstanceStateChange struct {
	InstanceID    string        `xml:"instanceId"`
	CurrentState  InstanceState `xml:"currentState"`
	PreviousState InstanceState `xml:"previousState"`
}

// --- Response envelopes -----------------------------------------------------

type RunInstancesResponse struct {
	XMLName       xml.Name   `xml:"RunInstancesResponse"`
	XMLNS         string     `xml:"xmlns,attr"`
	RequestID     string     `xml:"requestId"`
	ReservationID string     `xml:"reservationId"`
	OwnerID       string     `xml:"ownerId"`
	Instances     []Instance `xml:"instancesSet>item"`
}

type DescribeInstancesResponse struct {
	XMLName      xml.Name      `xml:"DescribeInstancesResponse"`
	XMLNS        string        `xml:"xmlns,attr"`
	RequestID    string        `xml:"requestId"`
	Reservations []Reservation `xml:"reservationSet>item"`
}

type StopInstancesResponse struct {
	XMLName   xml.Name              `xml:"StopInstancesResponse"`
	XMLNS     string                `xml:"xmlns,attr"`
	RequestID string                `xml:"requestId"`
	Changes   []InstanceStateChange `xml:"instancesSet>item"`
}

type StartInstancesResponse struct {
	XMLName   xml.Name              `xml:"StartInstancesResponse"`
	XMLNS     string                `xml:"xmlns,attr"`
	RequestID string                `xml:"requestId"`
	Changes   []InstanceStateChange `xml:"instancesSet>item"`
}

type TerminateInstancesResponse struct {
	XMLName   xml.Name              `xml:"TerminateInstancesResponse"`
	XMLNS     string                `xml:"xmlns,attr"`
	RequestID string                `xml:"requestId"`
	Changes   []InstanceStateChange `xml:"instancesSet>item"`
}

type RebootInstancesResponse struct {
	XMLName   xml.Name `xml:"RebootInstancesResponse"`
	XMLNS     string   `xml:"xmlns,attr"`
	RequestID string   `xml:"requestId"`
	Return    bool     `xml:"return"`
}

type CancelSpotInstanceRequestsResponse struct {
	XMLName   xml.Name `xml:"CancelSpotInstanceRequestsResponse"`
	XMLNS     string   `xml:"xmlns,attr"`
	RequestID string   `xml:"requestId"`
	// spotInstanceRequestSet is empty — brahmi doesn't inspect the result.
}

type DescribeInstanceAttributeResponse struct {
	XMLName    xml.Name `xml:"DescribeInstanceAttributeResponse"`
	XMLNS      string   `xml:"xmlns,attr"`
	RequestID  string   `xml:"requestId"`
	InstanceID string   `xml:"instanceId"`
}

// DescribeSubnetsResponse / DescribeSecurityGroupsResponse are empty-set
// envelopes — the mock returns them so the actions don't 501, but there is
// no docker concept to enumerate. Callers that pass tag selectors will get
// a "no match" error on their side, which is the intended signal.
type DescribeSubnetsResponse struct {
	XMLName   xml.Name `xml:"DescribeSubnetsResponse"`
	XMLNS     string   `xml:"xmlns,attr"`
	RequestID string   `xml:"requestId"`
	// subnetSet omitted → empty
}

type DescribeSecurityGroupsResponse struct {
	XMLName   xml.Name `xml:"DescribeSecurityGroupsResponse"`
	XMLNS     string   `xml:"xmlns,attr"`
	RequestID string   `xml:"requestId"`
	// securityGroupInfo omitted → empty
}

// ErrorResponse is what we write on any handler-level failure. AWS emits this
// wrapping a single <Errors><Error><Code/><Message/></Error></Errors> block.
type ErrorResponse struct {
	XMLName   xml.Name `xml:"Response"`
	Errors    []Error  `xml:"Errors>Error"`
	RequestID string   `xml:"RequestID"`
}

// Error is one entry in ErrorResponse.
type Error struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// writeXML serializes v to the response with correct headers.
func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "text/xml; charset=UTF-8")
	w.WriteHeader(status)
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		log.Printf("ec2mock: write header: %v", err)
		return
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Printf("ec2mock: xml encode: %v", err)
	}
}

// writeError emits an AWS-shaped error response.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeXML(w, status, ErrorResponse{
		Errors:    []Error{{Code: code, Message: msg}},
		RequestID: newRequestID(),
	})
}

