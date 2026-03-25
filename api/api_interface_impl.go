//go:generate go tool oapi-codegen --config=types.cfg.yaml ../docs/api/openapi.yaml
//go:generate go tool oapi-codegen --config=server.cfg.yaml ../docs/api/openapi.yaml
//go:generate go tool oapi-codegen --config=client.cfg.yaml ../docs/api/openapi.yaml

package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/util"
	"github.com/go-chi/chi/v5"
	"github.com/miekg/dns"
)

type httpReqCtxKey struct{}

// BlockingStatus represents the current blocking status
type BlockingStatus struct {
	// True if blocking is enabled
	Enabled bool
	// Disabled group names
	DisabledGroups []string
	// If blocking is temporarily disabled: amount of seconds until blocking will be enabled
	AutoEnableInSec int
}

// BlockingControl interface to control the blocking status
type BlockingControl interface {
	EnableBlocking(ctx context.Context)
	DisableBlocking(ctx context.Context, duration time.Duration, disableGroups []string) error
	BlockingStatus() BlockingStatus
}

// ListRefresher interface to control the list refresh
type ListRefresher interface {
	RefreshLists(ctx context.Context) error
}

type Querier interface {
	Query(
		ctx context.Context, serverHost string, clientIP net.IP, question string, qType dns.Type,
	) (*model.Response, error)
}

type CacheControl interface {
	FlushCaches(ctx context.Context)
}

// CustomDNSControl interface to manage custom DNS records
type CustomDNSControl interface {
	AddDNSRecord(domain string, recType string, value string, ttl uint32) error
	RemoveDNSRecord(domain string, recType string) error
	UpdateDNSRecord(domain string, records []ApiDNSRecord) error
	DeleteDNSRecord(domain string) error
	GetDNSRecords() []ApiDNSRecord
}

func RegisterOpenAPIEndpoints(router chi.Router, impl StrictServerInterface) {
	middleware := []StrictMiddlewareFunc{ctxWithHTTPRequestMiddleware}

	HandlerFromMuxWithBaseURL(NewStrictHandler(impl, middleware), router, "/api")
}

func ctxWithHTTPRequestMiddleware(handler StrictHandlerFunc, operationID string) StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (response any, err error) {
		ctx = context.WithValue(ctx, httpReqCtxKey{}, r)

		return handler(ctx, w, r, request)
	}
}

type OpenAPIInterfaceImpl struct {
	control      BlockingControl
	querier      Querier
	refresher    ListRefresher
	cacheControl CacheControl
	customDNS    CustomDNSControl
}

func NewOpenAPIInterfaceImpl(control BlockingControl,
	querier Querier,
	refresher ListRefresher,
	cacheControl CacheControl,
	customDNS CustomDNSControl,
) *OpenAPIInterfaceImpl {
	return &OpenAPIInterfaceImpl{
		control:      control,
		querier:      querier,
		refresher:    refresher,
		cacheControl: cacheControl,
		customDNS:    customDNS,
	}
}

func (i *OpenAPIInterfaceImpl) DisableBlocking(ctx context.Context,
	request DisableBlockingRequestObject,
) (DisableBlockingResponseObject, error) {
	var (
		duration time.Duration
		groups   []string
		err      error
	)

	if request.Params.Duration != nil {
		duration, err = time.ParseDuration(*request.Params.Duration)
		if err != nil {
			return DisableBlocking400TextResponse(log.EscapeInput(err.Error())), nil
		}
	}

	if request.Params.Groups != nil && len(*request.Params.Groups) > 0 {
		groups = strings.Split(*request.Params.Groups, ",")
	}

	err = i.control.DisableBlocking(ctx, duration, groups)
	if err != nil {
		return DisableBlocking400TextResponse(log.EscapeInput(err.Error())), nil
	}

	return DisableBlocking200Response{}, nil
}

func (i *OpenAPIInterfaceImpl) EnableBlocking(ctx context.Context, _ EnableBlockingRequestObject,
) (EnableBlockingResponseObject, error) {
	i.control.EnableBlocking(ctx)

	return EnableBlocking200Response{}, nil
}

func (i *OpenAPIInterfaceImpl) BlockingStatus(_ context.Context, _ BlockingStatusRequestObject,
) (BlockingStatusResponseObject, error) {
	blStatus := i.control.BlockingStatus()

	result := ApiBlockingStatus{
		Enabled: blStatus.Enabled,
	}

	if blStatus.AutoEnableInSec > 0 {
		result.AutoEnableInSec = &blStatus.AutoEnableInSec
	}

	if len(blStatus.DisabledGroups) > 0 {
		result.DisabledGroups = &blStatus.DisabledGroups
	}

	return BlockingStatus200JSONResponse(result), nil
}

func (i *OpenAPIInterfaceImpl) ListRefresh(ctx context.Context,
	_ ListRefreshRequestObject,
) (ListRefreshResponseObject, error) {
	err := i.refresher.RefreshLists(ctx)
	if err != nil {
		return ListRefresh500TextResponse(log.EscapeInput(err.Error())), nil
	}

	return ListRefresh200Response{}, nil
}

func (i *OpenAPIInterfaceImpl) Query(ctx context.Context, request QueryRequestObject) (QueryResponseObject, error) {
	qType := dns.Type(dns.StringToType[request.Body.Type])
	if qType == dns.Type(dns.TypeNone) {
		return Query400TextResponse(fmt.Sprintf("unknown query type '%s'", request.Body.Type)), nil
	}

	var (
		serverHost string
		clientIP   net.IP
	)

	httpReq, ok := ctx.Value(httpReqCtxKey{}).(*http.Request)
	if ok {
		serverHost = httpReq.Host
		clientIP = util.HTTPClientIP(httpReq)
	}

	resp, err := i.querier.Query(ctx, serverHost, clientIP, dns.Fqdn(request.Body.Query), qType)
	if err != nil {
		return nil, fmt.Errorf("query failed for '%s' (type %s): %w", request.Body.Query, request.Body.Type, err)
	}

	return Query200JSONResponse(ApiQueryResult{
		Reason:       resp.Reason,
		ResponseType: resp.RType.String(),
		Response:     util.AnswerToString(resp.Res.Answer),
		ReturnCode:   dns.RcodeToString[resp.Res.Rcode],
	}), nil
}

func (i *OpenAPIInterfaceImpl) CacheFlush(ctx context.Context,
	_ CacheFlushRequestObject,
) (CacheFlushResponseObject, error) {
	i.cacheControl.FlushCaches(ctx)

	return CacheFlush200Response{}, nil
}

func (i *OpenAPIInterfaceImpl) GetDnsRecords(_ context.Context,
	_ GetDnsRecordsRequestObject,
) (GetDnsRecordsResponseObject, error) {
	records := i.customDNS.GetDNSRecords()

	return GetDnsRecords200JSONResponse{Records: &records}, nil
}

func (i *OpenAPIInterfaceImpl) AddDnsRecord(_ context.Context,
	request AddDnsRecordRequestObject,
) (AddDnsRecordResponseObject, error) {
	if request.Body == nil {
		return AddDnsRecord400TextResponse("request body is required"), nil
	}

	var ttl uint32
	if request.Body.Ttl != nil {
		ttl = uint32(*request.Body.Ttl)
	}

	err := i.customDNS.AddDNSRecord("", string(request.Body.Type), request.Body.Value, ttl)
	if err != nil {
		return AddDnsRecord400TextResponse(log.EscapeInput(err.Error())), nil
	}

	return AddDnsRecord200Response{}, nil
}

func (i *OpenAPIInterfaceImpl) DeleteDnsRecord(_ context.Context,
	request DeleteDnsRecordRequestObject,
) (DeleteDnsRecordResponseObject, error) {
	err := i.customDNS.DeleteDNSRecord(request.Domain)
	if err != nil {
		return DeleteDnsRecord404TextResponse(log.EscapeInput(err.Error())), nil
	}

	return DeleteDnsRecord200Response{}, nil
}

func (i *OpenAPIInterfaceImpl) UpdateDnsRecord(_ context.Context,
	request UpdateDnsRecordRequestObject,
) (UpdateDnsRecordResponseObject, error) {
	if request.Body == nil {
		return UpdateDnsRecord400TextResponse("request body is required"), nil
	}

	var ttl uint32
	if request.Body.Ttl != nil {
		ttl = uint32(*request.Body.Ttl)
	}

	records := []ApiDNSRecord{
		{
			Type:  string(request.Body.Type),
			Value: request.Body.Value,
		},
	}
	if ttl > 0 {
		ttlInt := int(ttl)
		records[0].Ttl = &ttlInt
	}

	err := i.customDNS.UpdateDNSRecord(request.Domain, records)
	if err != nil {
		return UpdateDnsRecord400TextResponse(log.EscapeInput(err.Error())), nil
	}

	return UpdateDnsRecord200Response{}, nil
}
