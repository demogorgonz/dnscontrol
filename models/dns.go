package models

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"reflect"
	"strconv"
	"strings"

	"github.com/StackExchange/dnscontrol/transform"
	"github.com/miekg/dns"
)

const DefaultTTL = uint32(300)

type DNSConfig struct {
	Registrars   []*RegistrarConfig   `json:"registrars"`
	DNSProviders []*DNSProviderConfig `json:"dns_providers"`
	Domains      []*DomainConfig      `json:"domains"`
}

func (config *DNSConfig) FindDomain(query string) *DomainConfig {
	for _, b := range config.Domains {
		if b.Name == query {
			return b
		}
	}
	return nil
}

type RegistrarConfig struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Metadata json.RawMessage `json:"meta,omitempty"`
}

type DNSProviderConfig struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Metadata json.RawMessage `json:"meta,omitempty"`
}

// RecordConfig stores a DNS record.
// Providers are responsible for validating or normalizing the data
// that goes into a RecordConfig.
// If you update Name, you have to update NameFQDN and vice-versa.
//
// Name:
//    This is the shortname i.e. the NameFQDN without the origin suffix.
//    It should never have a trailing "."
//    It should never be null. It should store It "@", not the apex domain, not null, etc.
//    It shouldn't end with the domain origin. If the origin is "foo.com." then
//       if Name == "foo.com" then that literally means "foo.com.foo.com." is
//       the intended FQDN.
// NameFQDN:
//    This is the FQDN version of Name.
//    It should never have a trailiing ".".
type RecordConfig struct {
	Type     string            `json:"type"`
	Name     string            `json:"name"`   // The short name. See below.
	Target   string            `json:"target"` // If a name, must end with "."
	TTL      uint32            `json:"ttl,omitempty"`
	Metadata map[string]string `json:"meta,omitempty"`
	NameFQDN string            `json:"-"` // Must end with ".$origin". See below.
	Priority uint16            `json:"priority,omitempty"`

	Original interface{} `json:"-"` // Store pointer to provider-specific record object. Used in diffing.
}

func (r *RecordConfig) String() string {
	content := fmt.Sprintf("%s %s %s %d", r.Type, r.NameFQDN, r.Target, r.TTL)
	if r.Type == "MX" {
		content += fmt.Sprintf(" priority=%d", r.Priority)
	}
	for k, v := range r.Metadata {
		content += fmt.Sprintf(" %s=%s", k, v)
	}
	return content
}

/// Convert RecordConfig -> dns.RR.
func (r *RecordConfig) RR() dns.RR {

	// Note: The label is a FQDN ending in a ".".  It will not put "@" in the Name field.

	// NB(tlim): An alternative way to do this would be
	// to create the rr via: rr := TypeToRR[x]()
	// then set the parameters. A benchmark may find that
	// faster. This was faster to implement.

	rdtype, ok := dns.StringToType[r.Type]
	if !ok {
		log.Fatalf("No such DNS type as (%#v)\n", r.Type)
	}

	hdr := dns.RR_Header{
		Name:   r.NameFQDN + ".",
		Rrtype: rdtype,
		Class:  dns.ClassINET,
		Ttl:    r.TTL,
	}

	// Handle some special cases:
	switch rdtype {
	case dns.TypeMX:
		// Has a Priority field.
		return &dns.MX{Hdr: hdr, Preference: r.Priority, Mx: r.Target}
	case dns.TypeTXT:
		// Assure no problems due to quoting/unquoting:
		return &dns.TXT{Hdr: hdr, Txt: []string{r.Target}}
	default:
	}

	var ttl string
	if r.TTL == 0 {
		ttl = strconv.FormatUint(uint64(DefaultTTL), 10)
	} else {
		ttl = strconv.FormatUint(uint64(r.TTL), 10)
	}

	s := fmt.Sprintf("%s %s IN %s %s", r.NameFQDN, ttl, r.Type, r.Target)
	rc, err := dns.NewRR(s)
	if err != nil {
		log.Fatalf("NewRR rejected RecordConfig: %#v (t=%#v)\n%v\n", s, r.Target, err)
	}
	return rc
}

// Key gets the RecordKey for a record, suitable for grouping on type/name.
func (r *RecordConfig) Key() RecordKey {
	return RecordKey{Type: r.Type, Name: r.Name}
}

// InlineMXPriority will add the mx priority to the front of the record content.
// Use this for providers that require the priority and target in a single field.
func (r *RecordConfig) InlineMXPriority() {
	if r.Type == "MX" && r.Priority != 0 && !strings.Contains(r.Target, " ") {
		r.Target = fmt.Sprintf("%d %s", r.Priority, r.Target)
		r.Priority = 0
	}
}

// Records is a list of dns records
type Records []*RecordConfig

// RecordKey is used to group records by type and name
type RecordKey struct {
	Type string
	Name string
}

func (r Records) Grouped() map[RecordKey]Records {
	m := map[RecordKey]Records{}
	for _, rec := range r {
		k := rec.Key()
		m[k] = append(m[k], rec)
	}
	return m
}

type Nameserver struct {
	Name   string `json:"name"` // Normalized to a FQDN with NO trailing "."
	Target string `json:"target"`
}

func StringsToNameservers(nss []string) []*Nameserver {
	nservers := []*Nameserver{}
	for _, ns := range nss {
		nservers = append(nservers, &Nameserver{Name: ns})
	}
	return nservers
}

type DomainConfig struct {
	Name         string            `json:"name"` // NO trailing "."
	Registrar    string            `json:"registrar"`
	DNSProviders map[string]int    `json:"dnsProviders"`
	Metadata     map[string]string `json:"meta,omitempty"`
	Records      Records           `json:"records"`
	Nameservers  []*Nameserver     `json:"nameservers,omitempty"`
	KeepUnknown  bool              `json:"keepunknown"`
}

func (dc *DomainConfig) Copy() (*DomainConfig, error) {
	newDc := &DomainConfig{}
	err := copyObj(dc, newDc)
	return newDc, err
}

func (r *RecordConfig) Copy() (*RecordConfig, error) {
	newR := &RecordConfig{}
	err := copyObj(r, newR)
	return newR, err
}

func copyObj(input interface{}, output interface{}) error {
	buf := &bytes.Buffer{}
	enc := gob.NewEncoder(buf)
	dec := gob.NewDecoder(buf)
	if err := enc.Encode(input); err != nil {
		return err
	}
	if err := dec.Decode(output); err != nil {
		return err
	}
	return nil
}

func (dc *DomainConfig) HasRecordTypeName(rtype, name string) bool {
	for _, r := range dc.Records {
		if r.Type == rtype && r.Name == name {
			return true
		}
	}
	return false
}

func InterfaceToIP(i interface{}) (net.IP, error) {
	switch v := i.(type) {
	case float64:
		u := uint32(v)
		return transform.UintToIP(u), nil
	case string:
		if ip := net.ParseIP(v); ip != nil {
			return ip, nil
		}
		return nil, fmt.Errorf("%s is not a valid ip address", v)
	default:
		return nil, fmt.Errorf("Cannot convert type %s to ip.", reflect.TypeOf(i))
	}
}

//Correction is anything that can be run. Implementation is up to the specific provider.
type Correction struct {
	F   func() error `json:"-"`
	Msg string
}
