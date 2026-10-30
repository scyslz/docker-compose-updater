package main

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Image proxies are prefix proxies for container registries, such as
// https://gh-proxy.com/docker. An image reference is accelerated by putting the
// proxy prefix in front of it (gh-proxy.com/docker/nginx:alpine). Because
// Compose itself has no notion of a pull prefix, the updater pulls the
// accelerated reference with `docker pull` and re-tags the result under the
// name the compose file expects; `compose up` then reuses the local image.

// normalizeProxyURL validates and canonicalizes an image proxy URL. A missing
// scheme defaults to https, and a trailing slash is dropped so concatenation is
// predictable.
func normalizeProxyURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("proxy URL is required")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("invalid proxy URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("proxy URL must use http or https")
	}
	if u.Host == "" {
		return "", errors.New("proxy URL has no host")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("proxy URL must not contain credentials, a query, or a fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// proxyHostPath returns the host and path prefix of a proxy without its scheme,
// e.g. "gh-proxy.com/docker" for "https://gh-proxy.com/docker". It is used to
// recognize image references that already point at the proxy.
func proxyHostPath(proxy string) string {
	u, err := url.Parse(proxy)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Host
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		host += p
	}
	return host
}

// splitImageRegistry separates an image reference into its registry host and
// the remaining repository[:tag][@digest]. A reference without a registry
// (nginx:latest) yields an empty registry, which means Docker Hub.
func splitImageRegistry(image string) (registry, rest string) {
	i := strings.IndexByte(image, '/')
	if i < 0 {
		return "", image
	}
	first := image[:i]
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		return first, image[i+1:]
	}
	return "", image
}

// proxiedImageRef maps an image reference to its accelerated form using the
// given proxy prefix. References Docker cannot re-tag (digest-pinned images) or
// images that already use the proxy are returned unchanged.
func proxiedImageRef(proxy, image string) string {
	proxy = strings.TrimRight(strings.TrimSpace(proxy), "/")
	image = strings.TrimSpace(image)
	if proxy == "" || image == "" {
		return image
	}
	// A digest reference cannot be re-tagged under its original name, so it is
	// pulled directly instead of through the proxy.
	if strings.Contains(image, "@") {
		return image
	}
	if hp := proxyHostPath(proxy); hp != "" && strings.HasPrefix(image, hp+"/") {
		return image
	}
	registry, rest := splitImageRegistry(image)
	switch registry {
	case "", "docker.io", "index.docker.io", "registry-1.docker.io":
		// Official images live in the implicit "library" namespace; the public
		// prefixes expect "nginx:alpine" rather than "library/nginx:alpine".
		rest = strings.TrimPrefix(rest, "library/")
		return proxy + "/" + rest
	default:
		// Other registries keep their host in the proxy path, e.g.
		// https://gh-proxy.com/docker/ghcr.io/owner/app:tag.
		return proxy + "/" + image
	}
}

// projectImages lists the image references a project would pull. Services with
// a build section are excluded because Compose skips them during `pull` (their
// image, if any, is produced locally). The result is sorted and de-duplicated.
func projectImages(cfg composeConfig) []string {
	set := map[string]bool{}
	for _, svc := range cfg.Services {
		if hasBuild(svc.Build) {
			continue
		}
		if img := strings.TrimSpace(svc.Image); img != "" {
			set[img] = true
		}
	}
	images := make([]string, 0, len(set))
	for img := range set {
		images = append(images, img)
	}
	sort.Strings(images)
	return images
}
