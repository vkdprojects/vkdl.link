package server

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/vkdprojects/vkdl.link/internal/ident"
)

type ctxKey int

const ipHashKey ctxKey = 0

// clientIP descobre o IP real do cliente sem confiar em cabeçalhos forjáveis.
//
//  1. "Borda" é quem se conectou à nossa infraestrutura: o RemoteAddr quando ele é público;
//     quando é privado/loopback (um proxy interno, como o Traefik do Coolify), o ÚLTIMO
//     X-Forwarded-For, que é o salto que esse proxy acrescentou e o cliente não controla.
//  2. Só quando a borda está em `trusted` (as faixas da Cloudflare) o cabeçalho confiável
//     (CF-Connecting-IP) é usado. Fora disso, o IP é a própria borda: quem fala direto
//     com a origem não escolhe o IP com que é limitado.
//
// Valores que não são IP são ignorados.
func clientIP(r *http.Request, trustedHeader string, trusted []netip.Prefix) string {
	edge := edgeIP(r)
	if !edge.IsValid() {
		return "unknown"
	}
	if trustedHeader != "" && inAny(edge, trusted) {
		if ip, ok := parseAddr(r.Header.Get(trustedHeader)); ok {
			return ip.String()
		}
	}
	return edge.String()
}

func edgeIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote, ok := parseAddr(host)
	if !ok {
		return netip.Addr{}
	}
	if !isInternal(remote) {
		return remote
	}
	// Atrás de um proxy interno: o último salto do X-Forwarded-For foi escrito por ele.
	hops := r.Header.Values("X-Forwarded-For")
	if len(hops) == 0 {
		return remote
	}
	parts := strings.Split(hops[len(hops)-1], ",")
	if last, ok := parseAddr(parts[len(parts)-1]); ok {
		return last
	}
	return remote
}

func isInternal(a netip.Addr) bool {
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsUnspecified()
}

func inAny(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func parseAddr(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// withIPHash guarda no contexto o HMAC do IP do cliente; o IP cru não sai daqui.
func withIPHash(r *http.Request, hash string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ipHashKey, hash))
}

func ipHashFrom(r *http.Request) string {
	h, _ := r.Context().Value(ipHashKey).(string)
	return h
}

func (s *Server) hashIP(r *http.Request) string {
	return ident.HashIP(clientIP(r, s.opt.TrustedProxyHeader, s.opt.TrustedProxies), s.opt.Secret)
}
