package resolver

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"

	"github.com/0xERR0R/blocky/api"
	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/util"

	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

type createAnswerFunc func(question dns.Question, ip net.IP, ttl uint32) (dns.RR, error)

// extractIPFromRecord extracts the IP address from A or AAAA records, returns nil for other types
func extractIPFromRecord(entry dns.RR) net.IP {
	switch v := entry.(type) {
	case *dns.A:
		return v.A
	case *dns.AAAA:
		return v.AAAA
	}

	return nil
}

// CustomDNSResolver resolves passed domain name to ip address defined in domain-IP map
type CustomDNSResolver struct {
	configurable[*config.CustomDNS]
	NextResolver
	typed

	createAnswerFromQuestion createAnswerFunc
	mapping                  config.CustomDNSMapping
	reverseAddresses         map[string][]string
	mu                       sync.RWMutex
}

// NewCustomDNSResolver creates new resolver instance
func NewCustomDNSResolver(cfg config.CustomDNS) *CustomDNSResolver {
	dnsRecords := make(config.CustomDNSMapping, len(cfg.Mapping)+len(cfg.Zone.RRs))

	for url, entries := range cfg.Mapping {
		url = util.ExtractDomainOnly(url)
		dnsRecords[url] = entries

		for _, entry := range entries {
			entry.Header().Ttl = cfg.CustomTTL.SecondsU32()
		}
	}

	for url, entries := range cfg.Zone.RRs {
		url = util.ExtractDomainOnly(url)
		dnsRecords[url] = entries
	}

	// Load dynamic records from file
	dynamicRecords, err := cfg.LoadDynamicRecords()
	if err != nil {
		log.Log().WithError(err).Error("failed to load dynamic DNS records")
	} else if dynamicRecords != nil {
		for url, entries := range dynamicRecords {
			url = util.ExtractDomainOnly(url) // Normalize domain (remove trailing dot)
			if _, exists := dnsRecords[url]; !exists {
				dnsRecords[url] = entries
			} else {
				dnsRecords[url] = append(dnsRecords[url], entries...)
			}
		}
	}

	reverse := make(map[string][]string, len(dnsRecords))

	for url, entries := range dnsRecords {
		for _, entry := range entries {
			if ip := extractIPFromRecord(entry); ip != nil {
				r, _ := dns.ReverseAddr(ip.String())
				reverse[r] = append(reverse[r], url)
			}
		}
	}

	return &CustomDNSResolver{
		configurable: withConfig(&cfg),
		typed:        withType("custom_dns"),

		createAnswerFromQuestion: util.CreateAnswerFromQuestion,
		mapping:                  dnsRecords,
		reverseAddresses:         reverse,
	}
}

func isSupportedType(ip net.IP, question dns.Question) bool {
	return (ip.To4() != nil && question.Qtype == dns.TypeA) ||
		(strings.Contains(ip.String(), ":") && question.Qtype == dns.TypeAAAA)
}

func (r *CustomDNSResolver) handleReverseDNS(request *model.Request) *model.Response {
	question := request.Req.Question[0]
	if question.Qtype == dns.TypePTR {
		urls, found := r.reverseAddresses[question.Name]
		if found {
			answers := make([]dns.RR, 0, len(urls))
			for _, url := range urls {
				h := util.CreateHeader(question, r.cfg.CustomTTL.SecondsU32())
				ptr := new(dns.PTR)
				ptr.Ptr = dns.Fqdn(url)
				ptr.Hdr = h
				answers = append(answers, ptr)
			}

			return model.NewResponseWithAnswers(request, answers, model.ResponseTypeCUSTOMDNS, "CUSTOM DNS")
		}
	}

	return nil
}

func (r *CustomDNSResolver) processRequest(
	ctx context.Context,
	logger *logrus.Entry,
	request *model.Request,
	resolvedCnames []string,
) (*model.Response, error) {
	question := request.Req.Question[0]
	domain := util.ExtractDomain(question)
	var answers []dns.RR

	for len(domain) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("context cancelled during custom DNS resolution: %w", err)
		}

		entries, found := r.mapping[domain]

		if found {
			for _, entry := range entries {
				result, err := r.processDNSEntry(ctx, logger, request, resolvedCnames, question, entry)
				if err != nil {
					return nil, err
				}

				answers = append(answers, result...)
			}

			if len(answers) > 0 {
				logger.WithFields(logrus.Fields{
					"answer": util.AnswerToString(answers),
					"domain": domain,
				}).Debugf("returning custom dns entry")

				return model.NewResponseWithAnswers(request, answers, model.ResponseTypeCUSTOMDNS, "CUSTOM DNS"), nil
			}

			// Mapping exists for this domain, but for another type
			if !r.cfg.FilterUnmappedTypes {
				// go to next resolver
				break
			}

			// return NOERROR with empty result
			return model.NewResponseWithReason(request, model.ResponseTypeCUSTOMDNS, "CUSTOM DNS"), nil
		}

		if i := strings.IndexRune(domain, '.'); i >= 0 {
			domain = domain[i+1:]
		} else {
			break
		}
	}

	logger.WithField("next_resolver", Name(r.next)).Trace("go to next resolver")

	return r.next.Resolve(ctx, request)
}

func (r *CustomDNSResolver) processDNSEntry(
	ctx context.Context,
	logger *logrus.Entry,
	request *model.Request,
	resolvedCnames []string,
	question dns.Question,
	entry dns.RR,
) ([]dns.RR, error) {
	switch v := entry.(type) {
	case *dns.A:
		return r.processIP(v.A, question, v.Header().Ttl)
	case *dns.AAAA:
		return r.processIP(v.AAAA, question, v.Header().Ttl)
	case *dns.TXT:
		return r.processTXT(v.Txt, question, v.Header().Ttl)
	case *dns.SRV:
		return r.processSRV(*v, question, v.Header().Ttl)
	case *dns.CNAME:
		return r.processCNAME(ctx, logger, request, *v, resolvedCnames, question, v.Header().Ttl)
	}

	return nil, fmt.Errorf("unsupported customDNS RR type %T", entry)
}

// Resolve uses internal mapping to resolve the query
func (r *CustomDNSResolver) Resolve(ctx context.Context, request *model.Request) (*model.Response, error) {
	ctx, logger := r.log(ctx)

	r.mu.RLock()
	defer r.mu.RUnlock()

	reverseResp := r.handleReverseDNS(request)
	if reverseResp != nil {
		return reverseResp, nil
	}

	// Apply domain rewrites if configured
	original := request.Req
	rewritten, originalNames := rewriteRequest(logger, original, r.cfg.Rewrite)
	if rewritten != nil {
		request.Req = rewritten
	}

	response, err := r.processRequest(ctx, logger, request, make([]string, 0, len(r.mapping)))

	// Revert the request
	request.Req = original

	if err != nil {
		return response, err
	}

	// Revert rewrites in the response
	if rewritten != nil && response != NoResponse && response.Res != nil {
		revertRewritesInResponse(response.Res, originalNames)
	}

	return response, nil
}

func (r *CustomDNSResolver) processIP(ip net.IP, question dns.Question, ttl uint32) (result []dns.RR, err error) {
	result = make([]dns.RR, 0)

	if isSupportedType(ip, question) {
		rr, err := r.createAnswerFromQuestion(question, ip, ttl)
		if err != nil {
			return nil, err
		}

		result = append(result, rr)
	}

	return result, nil
}

func (r *CustomDNSResolver) processTXT(value []string, question dns.Question, ttl uint32) (result []dns.RR, err error) {
	if question.Qtype == dns.TypeTXT {
		txt := new(dns.TXT)
		txt.Hdr = dns.RR_Header{Class: dns.ClassINET, Ttl: ttl, Rrtype: dns.TypeTXT, Name: question.Name}
		txt.Txt = value
		result = append(result, txt)
	}

	return result, nil
}

func (r *CustomDNSResolver) processSRV(
	targetSRV dns.SRV,
	question dns.Question,
	ttl uint32,
) (result []dns.RR, err error) {
	if question.Qtype == dns.TypeSRV {
		srv := new(dns.SRV)
		srv.Hdr = dns.RR_Header{Class: dns.ClassINET, Ttl: ttl, Rrtype: dns.TypeSRV, Name: question.Name}
		srv.Priority = targetSRV.Priority
		srv.Weight = targetSRV.Weight
		srv.Port = targetSRV.Port
		srv.Target = targetSRV.Target
		result = append(result, srv)
	}

	return result, nil
}

func (r *CustomDNSResolver) processCNAME(
	ctx context.Context,
	logger *logrus.Entry,
	request *model.Request,
	targetCname dns.CNAME,
	resolvedCnames []string,
	question dns.Question,
	ttl uint32,
) (result []dns.RR, err error) {
	cname := new(dns.CNAME)
	cname.Hdr = dns.RR_Header{Class: dns.ClassINET, Ttl: ttl, Rrtype: dns.TypeCNAME, Name: question.Name}
	cname.Target = dns.Fqdn(targetCname.Target)
	result = append(result, cname)

	if question.Qtype == dns.TypeCNAME {
		return result, nil
	}

	targetWithoutDot := strings.TrimSuffix(targetCname.Target, ".")

	if slices.Contains(resolvedCnames, targetWithoutDot) {
		return nil, fmt.Errorf("CNAME loop detected: %v", append(resolvedCnames, targetWithoutDot))
	}

	cnames := resolvedCnames
	cnames = append(cnames, targetWithoutDot)

	clientIP := request.ClientIP.String()
	clientID := request.RequestClientID
	targetRequest := newRequestWithClientID(targetWithoutDot, dns.Type(question.Qtype), clientIP, clientID)

	// resolve the target recursively
	targetResp, err := r.processRequest(ctx, logger, targetRequest, cnames)
	if err != nil {
		return nil, err
	}

	// If target resolution returns NoResponse, just return the CNAME record itself
	if targetResp == NoResponse {
		return result, nil
	}

	result = append(result, targetResp.Res.Answer...)

	return result, nil
}

func (r *CustomDNSResolver) CreateAnswerFromQuestion(newFunc createAnswerFunc) {
	r.createAnswerFromQuestion = newFunc
}

// AddDNSRecord adds a new DNS record for a domain
func (r *CustomDNSResolver) AddDNSRecord(domain string, recType string, value string, ttl uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	domain = util.ExtractDomainOnly(domain)
	if ttl == 0 {
		ttl = r.cfg.CustomTTL.SecondsU32()
	}

	rr, err := r.cfg.RecordToRR(domain, recType, value, ttl)
	if err != nil {
		return err
	}

	// Add to mapping
	r.mapping[domain] = append(r.mapping[domain], rr)

	// Update reverse addresses for A/AAAA records
	if ip := extractIPFromRecord(rr); ip != nil {
		raddr, _ := dns.ReverseAddr(ip.String())
		r.reverseAddresses[raddr] = append(r.reverseAddresses[raddr], domain)
	}

	// Save to file
	return r.cfg.SaveDynamicRecords(r.mapping)
}

// RemoveDNSRecord removes a DNS record from a domain
func (r *CustomDNSResolver) RemoveDNSRecord(domain string, recType string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	domain = util.ExtractDomainOnly(domain)

	entries, found := r.mapping[domain]
	if !found {
		return fmt.Errorf("domain not found: %s", domain)
	}

	typeToRemove := dns.StringToType[recType]
	var newEntries []dns.RR

	for _, entry := range entries {
		if entry.Header().Rrtype == typeToRemove {
			// Remove from reverse addresses
			if ip := extractIPFromRecord(entry); ip != nil {
				raddr, _ := dns.ReverseAddr(ip.String())
				if addrs, ok := r.reverseAddresses[raddr]; ok {
					var filtered []string
					for _, a := range addrs {
						if a != domain {
							filtered = append(filtered, a)
						}
					}
					if len(filtered) == 0 {
						delete(r.reverseAddresses, raddr)
					} else {
						r.reverseAddresses[raddr] = filtered
					}
				}
			}
		} else {
			newEntries = append(newEntries, entry)
		}
	}

	if len(newEntries) == 0 {
		delete(r.mapping, domain)
	} else {
		r.mapping[domain] = newEntries
	}

	// Save to file
	return r.cfg.SaveDynamicRecords(r.mapping)
}

// UpdateDNSRecord replaces all DNS records for a domain
func (r *CustomDNSResolver) UpdateDNSRecord(domain string, records []api.ApiDNSRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	domain = util.ExtractDomainOnly(domain)

	// Remove old entries for this domain
	if oldEntries, found := r.mapping[domain]; found {
		for _, entry := range oldEntries {
			if ip := extractIPFromRecord(entry); ip != nil {
				raddr, _ := dns.ReverseAddr(ip.String())
				delete(r.reverseAddresses, raddr)
			}
		}
	}

	// Add new records
	for _, rec := range records {
		var ttl uint32
		if rec.Ttl != nil {
			ttl = uint32(*rec.Ttl)
		}
		if ttl == 0 {
			ttl = r.cfg.CustomTTL.SecondsU32()
		}
		rr, err := r.cfg.RecordToRR(domain, rec.Type, rec.Value, ttl)
		if err != nil {
			return err
		}
		r.mapping[domain] = append(r.mapping[domain], rr)

		if ip := extractIPFromRecord(rr); ip != nil {
			raddr, _ := dns.ReverseAddr(ip.String())
			r.reverseAddresses[raddr] = append(r.reverseAddresses[raddr], domain)
		}
	}

	// Save to file
	return r.cfg.SaveDynamicRecords(r.mapping)
}

// GetDNSRecords returns all DNS records
func (r *CustomDNSResolver) GetDNSRecords() []api.ApiDNSRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var records []api.ApiDNSRecord

	for domain, entries := range r.mapping {
		for _, entry := range entries {
			rec := api.ApiDNSRecord{
				Domain: domain,
			}

			switch v := entry.(type) {
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
				ttl := int(entry.Header().Ttl)
				rec.Ttl = &ttl
				records = append(records, rec)
			}
		}
	}

	return records
}

// DeleteDNSRecord deletes all DNS records for a domain
func (r *CustomDNSResolver) DeleteDNSRecord(domain string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	domain = util.ExtractDomainOnly(domain)

	entries, found := r.mapping[domain]
	if !found {
		return fmt.Errorf("domain not found: %s", domain)
	}

	// Remove from reverse addresses
	for _, entry := range entries {
		if ip := extractIPFromRecord(entry); ip != nil {
			raddr, _ := dns.ReverseAddr(ip.String())
			delete(r.reverseAddresses, raddr)
		}
	}

	delete(r.mapping, domain)

	// Save to file
	return r.cfg.SaveDynamicRecords(r.mapping)
}
