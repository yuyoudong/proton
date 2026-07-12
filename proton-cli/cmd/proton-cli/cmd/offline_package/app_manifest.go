package offline_package

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

func normalizeAppPlatform(platform string) (string, error) {
	platforms, err := normalizeAppPlatforms(platform)
	if err != nil {
		return "", err
	}
	return platforms[0], nil
}

func normalizeAppPlatforms(platform string) ([]string, error) {
	items := strings.Split(strings.TrimSpace(platform), ",")
	if len(items) == 1 && items[0] == "" {
		items = []string{"linux/amd64"}
	}

	seen := map[string]struct{}{}
	var out []string
	for _, item := range items {
		var normalized string
		switch strings.ToLower(strings.TrimSpace(item)) {
		case "", "linux/amd64", "amd64", "x86_64":
			normalized = "linux/amd64"
		case "linux/arm64", "arm64", "aarch64":
			normalized = "linux/arm64"
		default:
			return nil, fmt.Errorf("unsupported platform %q", strings.TrimSpace(item))
		}

		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no platform selected")
	}
	return out, nil
}

func loadAppManifest(ctx context.Context, path string) ([]byte, *appManifest, error) {
	body, err := readAppManifestSource(ctx, path)
	if err != nil {
		return nil, nil, err
	}

	var manifest appManifest
	if err := yaml.Unmarshal(body, &manifest); err != nil {
		return nil, nil, fmt.Errorf("parse manifest yaml: %w", err)
	}

	if err := validateAppManifest(&manifest); err != nil {
		return nil, nil, err
	}

	return body, &manifest, nil
}

func loadAppManifestTree(ctx context.Context, path string, includeDependencies bool) ([]byte, *appManifest, []appManifestDocument, error) {
	visited := map[string]struct{}{}
	documents := make([]appManifestDocument, 0, 4)
	if err := walkAppManifestTree(ctx, "", path, includeDependencies, visited, &documents); err != nil {
		return nil, nil, nil, err
	}
	if len(documents) == 0 {
		return nil, nil, nil, fmt.Errorf("manifest %q was not loaded", path)
	}

	root := documents[0]
	return root.Bytes, root.Manifest, documents, nil
}

func walkAppManifestTree(ctx context.Context, baseLocation, rawPath string, includeDependencies bool, visited map[string]struct{}, documents *[]appManifestDocument) error {
	location, err := resolveAppManifestLocation(baseLocation, rawPath)
	if err != nil {
		return err
	}
	if _, ok := visited[location]; ok {
		return nil
	}

	body, manifest, err := loadAppManifest(ctx, location)
	if err != nil {
		return err
	}
	visited[location] = struct{}{}
	*documents = append(*documents, appManifestDocument{
		Location: location,
		Bytes:    body,
		Manifest: manifest,
	})

	if !includeDependencies {
		return nil
	}

	for i, dep := range manifest.Dependencies {
		depManifest := strings.TrimSpace(dep.Manifest)
		if depManifest == "" {
			return fmt.Errorf("dependency %d (%s %s) manifest is required", i, dep.Product, dep.Version)
		}
		if err := walkAppManifestTree(ctx, location, depManifest, includeDependencies, visited, documents); err != nil {
			return fmt.Errorf("load dependency manifest %q for %s %s: %w", depManifest, dep.Product, dep.Version, err)
		}
	}

	return nil
}

func readAppManifestSource(ctx context.Context, path string) ([]byte, error) {
	if u, err := url.Parse(path); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
		if err != nil {
			return nil, err
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("download manifest: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("download manifest: status %s: %s", resp.Status, strings.TrimSpace(string(b)))
		}

		return io.ReadAll(resp.Body)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	return body, nil
}

func resolveAppManifestLocation(baseLocation, rawPath string) (string, error) {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return "", fmt.Errorf("manifest path is required")
	}

	if u, err := url.Parse(trimmed); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		return u.String(), nil
	}

	if baseLocation != "" {
		if baseURL, err := url.Parse(baseLocation); err == nil && (baseURL.Scheme == "http" || baseURL.Scheme == "https") {
			ref, err := url.Parse(trimmed)
			if err != nil {
				return "", fmt.Errorf("parse dependency manifest path %q: %w", trimmed, err)
			}
			return baseURL.ResolveReference(ref).String(), nil
		}

		if !filepath.IsAbs(trimmed) {
			trimmed = filepath.Join(filepath.Dir(baseLocation), trimmed)
		}
	}

	absPath, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve manifest path %q: %w", rawPath, err)
	}
	return absPath, nil
}

func validateAppManifest(manifest *appManifest) error {
	switch {
	case manifest.Kind != "VersionSet":
		return fmt.Errorf("manifest kind must be VersionSet")
	case manifest.Product == "":
		return fmt.Errorf("manifest product is required")
	case manifest.Version == "":
		return fmt.Errorf("manifest version is required")
	case manifest.Source.RepoURL() == "":
		return fmt.Errorf("manifest source.helmRepoUrl or source.registry is required")
	case len(manifest.Releases) == 0:
		return fmt.Errorf("manifest releases must not be empty")
	}

	for name, release := range manifest.Releases {
		if release.Chart == "" {
			return fmt.Errorf("release %q chart is required", name)
		}
		if release.Version == "" {
			return fmt.Errorf("release %q version is required", name)
		}
	}

	return nil
}

func collectAppChartRequests(documents []appManifestDocument) []appChartArtifact {
	seen := map[string]struct{}{}
	filenameCounts := map[string]int{}
	charts := make([]appChartArtifact, 0)

	for _, document := range documents {
		releases := make([]string, 0, len(document.Manifest.Releases))
		for name := range document.Manifest.Releases {
			releases = append(releases, name)
		}
		sort.Strings(releases)

		for _, releaseName := range releases {
			release := document.Manifest.Releases[releaseName]
			key := strings.Join([]string{document.Manifest.Source.RepoURL(), release.Chart, release.Version}, "\x00")
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}

			filename := fmt.Sprintf("%s-%s.tgz", release.Chart, release.Version)
			filenameCounts[filename]++
			if filenameCounts[filename] > 1 {
				filename = fmt.Sprintf("%s-%s-%d.tgz", release.Chart, release.Version, filenameCounts[filename])
			}

			charts = append(charts, appChartArtifact{
				Name:    release.Chart,
				Version: release.Version,
				RepoURL: document.Manifest.Source.RepoURL(),
				Path:    filename,
			})
		}
	}

	sort.Slice(charts, func(i, j int) bool {
		if charts[i].RepoURL != charts[j].RepoURL {
			return charts[i].RepoURL < charts[j].RepoURL
		}
		if charts[i].Name != charts[j].Name {
			return charts[i].Name < charts[j].Name
		}
		return charts[i].Version < charts[j].Version
	})

	return charts
}

func buildExportedManifest(manifestBytes []byte, platforms []string, overrideRegistry string, images []appPackageImage, imageErrors []string) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(manifestBytes, &doc); err != nil {
		return nil, fmt.Errorf("parse manifest for export: %w", err)
	}

	metadata := map[string]any{
		"platforms":        platforms,
		"overrideRegistry": strings.TrimSpace(overrideRegistry),
		"exportedAt":       timeNow().UTC().Format(time.RFC3339),
		"images":           images,
		"imageErrors":      imageErrors,
	}
	if len(platforms) == 1 {
		metadata["platform"] = platforms[0]
	}
	doc["offlinePackage"] = metadata

	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal exported manifest: %w", err)
	}
	return out, nil
}
