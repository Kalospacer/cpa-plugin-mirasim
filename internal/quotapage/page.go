// Package quotapage serves the Mirasim quota page as a plugin-owned resource
// route.
//
// CPA's quota capability is what Management Center reads through its own quota
// API, but the panel only renders quota adapters it was compiled with, so the
// plugin also publishes this page. It replicates the panel's own quota
// management view one for one — credential cards with health, plan and
// progress bars, plus the window grid — so the operator does not have to
// learn a second layout. Only resource routes can be embedded in the panel,
// and those routes are GET-only and unauthenticated, so the page lives at an
// unguessable path segment rather than an operator-visible one. The segment is
// generated once per process and published through the authenticated plugin
// list that the panel itself reads, so following it is no easier than reading
// that list; it does, however, appear in request logs and browser history.
// Package scope keeps it stable across the reconfigure CPA runs when it
// applies a config, so a save does not move the URL out from under an open
// panel iframe; only a real plugin reload, a new process, changes it.
package quotapage

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/credentials"
)

const (
	routePrefix = "/quota/"

	menuLabel       = "Mirasim Quota"
	menuDescription = "Mirasim account limits as reported by GET /v1/limits."
)

// QuotaFetcher reads the normalized limits for one credential. quota.Provider
// implements it, so the page renders the same answer the quota capability
// serves instead of reading limits a second way.
type QuotaFetcher interface {
	FetchQuota(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error)
}

// HostServices are the host callbacks a resource handler may reach through the
// host_callback_id the host supplies with the request. The page only reads the
// credential list, one credential's stored JSON, and the host HTTP client.
type HostServices interface {
	ListAuth(context.Context) ([]pluginapi.HostAuthFileEntry, error)
	GetAuth(context.Context, string) (pluginapi.HostAuthGetResponse, error)
	HTTPClient() pluginapi.HostHTTPClient
}

// Page is a quota page of one process. Every Page built in that process
// shares processSegment, so the URL the panel embeds survives the reconfigure
// CPA runs on each config apply; only a real plugin reload, a new process,
// changes it.
type Page struct {
	fetcher  QuotaFetcher
	segment  string
	services ProbeServices
	probes   *probeStore
}

// processSegment is generated once for the process, not once per Page: CPA
// re-runs plugin.Build on every config apply, and a segment generated there
// would move the page's URL out from under a panel iframe that is already open.
var processSegment = strings.ToLower(rand.Text())

// New builds the page. Probe services are optional: without them the page
// renders quota only and never publishes the model-availability panel, because
// running a probe needs the host's model and executor services.
func New(fetcher QuotaFetcher, services ...ProbeServices) *Page {
	p := &Page{fetcher: fetcher, segment: processSegment}
	if len(services) > 0 {
		p.services = services[0]
		p.probes = newProbeStore()
	}
	return p
}

// Resource is the route declaration the host turns into a menu entry. The path
// is relative: the host resolves it under the plugin's resource prefix.
func (p *Page) Resource() pluginapi.ResourceRoute {
	return pluginapi.ResourceRoute{
		Path:        routePrefix + p.segment,
		Menu:        menuLabel,
		Description: menuDescription,
	}
}

// Owns reports whether path addresses this page's route. Only a path under
// the /quota/ prefix whose final segment is the secret is claimed, not any
// path that happens to end in the secret; the segment is compared in constant
// time because it is the access control for an unauthenticated route.
func (p *Page) Owns(path string) bool {
	if p == nil || p.segment == "" {
		return false
	}
	idx := strings.LastIndex(path, routePrefix)
	if idx < 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(path[idx+len(routePrefix):]), []byte(p.segment)) == 1
}

// Serve renders the quota page, or answers a model-availability probe when the
// request carries the page's action parameter. Reading limits needs the host
// callbacks, so a request that reaches the handler without them answers 503
// rather than claiming the account has no limits. A credential whose limits
// cannot be read renders as unavailable on an otherwise successful page; only a
// request that addresses something other than this page answers 404.
func (p *Page) Serve(ctx context.Context, req pluginapi.ManagementRequest, host HostServices) (pluginapi.ManagementResponse, error) {
	if !strings.EqualFold(req.Method, http.MethodGet) {
		return renderResponse(http.StatusNotFound, pageView{Problem: problemNotFound})
	}
	if host == nil {
		return renderResponse(http.StatusServiceUnavailable, pageView{Problem: problemNoCallbacks})
	}
	client := host.HTTPClient()
	if client == nil {
		return renderResponse(http.StatusServiceUnavailable, pageView{Problem: problemNoCallbacks})
	}
	if req.Query.Get("action") != "" {
		return p.serveProbe(ctx, req, host)
	}
	return renderResponse(http.StatusOK, p.buildView(ctx, host, client))
}

// isMirasimAuth filters the host's credential list down to this plugin's own
// credentials before any credential JSON is read. The provider field is what
// the host derives from the auth's registered provider.
func isMirasimAuth(entry pluginapi.HostAuthFileEntry) bool {
	return strings.EqualFold(strings.TrimSpace(entry.Provider), credentials.Provider) ||
		strings.EqualFold(strings.TrimSpace(entry.Type), credentials.Provider)
}
