package config

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

// CustomDNS custom DNS configuration
type CustomDNS struct {
	RewriterConfig `yaml:",inline"`

	CustomTTL           Duration         `default:"1h"   yaml:"customTTL"`
	Mapping             CustomDNSMapping `yaml:"mapping"`
	Zone                ZoneFileDNS      `default:""     yaml:"zone"`
	FilterUnmappedTypes bool             `default:"true" yaml:"filterUnmappedTypes"`
	DynamicFilePath     string           `yaml:"dynamicFile"`
}

// DynamicDNSRecord represents a DNS record for YAML serialization
type DynamicDNSRecord struct {
	Type  string `yaml:"type"`
	Value string `yaml:"value"`
	TTL   uint32 `yaml:"ttl"`
}

// DynamicDNSFile represents the structure of the dynamic records YAML file
type DynamicDNSFile struct {
	Records map[string][]DynamicDNSRecord `yaml:"records"`
}

type (
	CustomDNSMapping map[string]CustomDNSEntries
	CustomDNSEntries []dns.RR

	ZoneFileDNS struct {
		RRs        CustomDNSMapping
		configPath string
	}
)

func (z *ZoneFileDNS) UnmarshalYAML(unmarshal func(any) error) error {
	var input string
	if err := unmarshal(&input); err != nil {
		return fmt.Errorf("failed to unmarshal zone file DNS: %w", err)
	}

	result := make(CustomDNSMapping)

	zoneParser := dns.NewZoneParser(strings.NewReader(input), "", z.configPath)
	zoneParser.SetIncludeAllowed(true)

	for {
		zoneRR, ok := zoneParser.Next()

		if !ok {
			if zoneParser.Err() != nil {
				return fmt.Errorf("zone file parsing error: %w", zoneParser.Err())
			}

			// Done
			break
		}

		domain := zoneRR.Header().Name

		if _, ok := result[domain]; !ok {
			result[domain] = make(CustomDNSEntries, 0, 1)
		}

		result[domain] = append(result[domain], zoneRR)
	}

	z.RRs = result

	return nil
}

func (c *CustomDNSEntries) UnmarshalYAML(unmarshal func(any) error) error {
	var input string
	if err := unmarshal(&input); err != nil {
		return fmt.Errorf("failed to unmarshal custom DNS entries: %w", err)
	}

	parts := strings.Split(input, ",")
	result := make(CustomDNSEntries, len(parts))

	for i, part := range parts {
		rr, err := configToRR(strings.TrimSpace(part))
		if err != nil {
			return fmt.Errorf("invalid custom DNS entry '%s': %w", part, err)
		}

		result[i] = rr
	}

	*c = result

	return nil
}

// IsEnabled implements `config.Configurable`.
func (c *CustomDNS) IsEnabled() bool {
	return len(c.Mapping) != 0 || c.DynamicFilePath != ""
}

// LogConfig implements `config.Configurable`.
func (c *CustomDNS) LogConfig(logger *logrus.Entry) {
	logger.Debugf("TTL = %s", c.CustomTTL)
	logger.Debugf("filterUnmappedTypes = %t", c.FilterUnmappedTypes)

	if c.DynamicFilePath != "" {
		logger.Infof("dynamicFile = %s", c.DynamicFilePath)
	}

	logger.Info("mapping:")

	for key, val := range c.Mapping {
		logger.Infof("  %s = %s", key, val)
	}
}

// SaveDynamicRecords saves the dynamic records to the YAML file
func (c *CustomDNS) SaveDynamicRecords(records CustomDNSMapping) error {
	if c.DynamicFilePath == "" {
		return nil
	}

	dynFile := DynamicDNSFile{
		Records: make(map[string][]DynamicDNSRecord),
	}

	for domain, entries := range records {
		for _, rr := range entries {
			rec := DynamicDNSRecord{
				TTL:   rr.Header().Ttl,
			}

			switch v := rr.(type) {
			case *dns.A:
				rec.Type = "A"
				rec.Value = v.A.String()
			case *dns.AAAA:
				rec.Type = "AAAA"
				rec.Value = v.AAAA.String()
			case *dns.TXT:
				rec.Type = "TXT"
				rec.Value = strings.Join(v.Txt, "")
			case *dns.CNAME:
				rec.Type = "CNAME"
				rec.Value = v.Target
			case *dns.SRV:
				rec.Type = "SRV"
				rec.Value = fmt.Sprintf("%d %d %d %s", v.Priority, v.Weight, v.Port, v.Target)
			case *dns.PTR:
				rec.Type = "PTR"
				rec.Value = v.Ptr
			}

			if rec.Type != "" {
				dynFile.Records[domain] = append(dynFile.Records[domain], rec)
			}
		}
	}

	data, err := yaml.Marshal(dynFile)
	if err != nil {
		return fmt.Errorf("failed to marshal dynamic records: %w", err)
	}

	err = os.WriteFile(c.DynamicFilePath, data, 0o600)
	if err != nil {
		return fmt.Errorf("failed to write dynamic records file: %w", err)
	}

	return nil
}

// LoadDynamicRecords loads the dynamic records from the YAML file
func (c *CustomDNS) LoadDynamicRecords() (map[string][]dns.RR, error) {
	if c.DynamicFilePath == "" {
		return nil, nil
	}

	data, err := os.ReadFile(c.DynamicFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read dynamic records file: %w", err)
	}

	var dynFile DynamicDNSFile
	err = yaml.Unmarshal(data, &dynFile)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal dynamic records: %w", err)
	}

	result := make(map[string][]dns.RR)
	for domain, records := range dynFile.Records {
		for _, rec := range records {
			rr, err := c.RecordToRR(domain, rec.Type, rec.Value, rec.TTL)
			if err != nil {
				continue // Skip invalid records
			}
			result[domain] = append(result[domain], rr)
		}
	}

	return result, nil
}

// RecordToRR creates a dns.RR from the given parameters
func (c *CustomDNS) RecordToRR(domain, recType, value string, ttl uint32) (dns.RR, error) {
	domain = dns.Fqdn(domain)

	var rr dns.RR

	switch strings.ToUpper(recType) {
	case "A":
		ip := net.ParseIP(value)
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("invalid A record value: %s", value)
		}
		a := new(dns.A)
		a.Hdr = dns.RR_Header{Name: domain, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl}
		a.A = ip
		rr = a

	case "AAAA":
		ip := net.ParseIP(value)
		if ip == nil || ip.To4() != nil {
			return nil, fmt.Errorf("invalid AAAA record value: %s", value)
		}
		aaaa := new(dns.AAAA)
		aaaa.Hdr = dns.RR_Header{Name: domain, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: ttl}
		aaaa.AAAA = ip
		rr = aaaa

	case "TXT":
		txt := new(dns.TXT)
		txt.Hdr = dns.RR_Header{Name: domain, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: ttl}
		txt.Txt = []string{value}
		rr = txt

	case "CNAME":
		cname := new(dns.CNAME)
		cname.Hdr = dns.RR_Header{Name: domain, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: ttl}
		cname.Target = dns.Fqdn(value)
		rr = cname

	case "SRV":
		var priority, weight, port uint16
		var target string
		_, err := fmt.Sscanf(value, "%d %d %d %s", &priority, &weight, &port, &target)
		if err != nil {
			return nil, fmt.Errorf("invalid SRV record value: %s", value)
		}
		srv := new(dns.SRV)
		srv.Hdr = dns.RR_Header{Name: domain, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: ttl}
		srv.Priority = priority
		srv.Weight = weight
		srv.Port = port
		srv.Target = dns.Fqdn(target)
		rr = srv

	case "PTR":
		ptr := new(dns.PTR)
		ptr.Hdr = dns.RR_Header{Name: domain, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: ttl}
		ptr.Ptr = dns.Fqdn(value)
		rr = ptr

	default:
		return nil, fmt.Errorf("unsupported record type: %s", recType)
	}

	return rr, nil
}

func configToRR(ipStr string) (dns.RR, error) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP address '%s'", ipStr)
	}

	if ip.To4() != nil {
		a := new(dns.A)
		a.A = ip

		return a, nil
	}

	aaaa := new(dns.AAAA)
	aaaa.AAAA = ip

	return aaaa, nil
}
