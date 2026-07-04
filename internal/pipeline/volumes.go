package pipeline

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"

	"github.com/ekho/gobackup-docker/internal/docker"
)

// Inspector can inspect a container by ID. Satisfied by *docker.Client.
type Inspector interface {
	ContainerInspect(ctx context.Context, id string) (docker.InspectResult, error)
}

// ArchiveVolumes collects the volume mounts needed in the gobackup container
// for a model with archive.includes, and returns the transformed includes that
// reference the mounted paths.
type ArchiveVolumes struct {
	ModelName string            // the model name, e.g. "nextcloud"
	Mounts    []docker.MountDef // volumes to mount into gobackup
	Includes  []string          // transformed archive.includes paths
	Excludes  []string          // transformed archive.excludes paths (if any)
}

// discoverArchiveVolumes inspects a source container and builds ArchiveVolumes.
// For each path in includes, it finds the matching MountPoint in the container's
// mounts. Each mount is added to the gobackup container at
// /volumes/<modelName>/<MountPoint.Destination>. The includes paths are
// transformed by prepending /volumes/<modelName>/.
func discoverArchiveVolumes(
	ctx context.Context,
	inspector Inspector,
	containerID string,
	modelName string,
	includes, excludes []string,
) (ArchiveVolumes, error) {
	if len(includes) == 0 {
		return ArchiveVolumes{ModelName: modelName}, nil
	}

	info, err := inspector.ContainerInspect(ctx, containerID)
	if err != nil {
		return ArchiveVolumes{}, fmt.Errorf("inspect source %s: %w", containerID[:min(len(containerID), 12)], err)
	}

	destMap := buildDestMap(info.Mounts)

	var mounts []docker.MountDef
	seen := map[string]bool{}

	transform := func(path string) (string, error) {
		mnt, mountDest, err := findMountForPath(path, destMap)
		if err != nil {
			return "", err
		}

		key := mnt.Source + ":" + mountDest
		if !seen[key] {
			seen[key] = true
			mounts = append(mounts, docker.MountDef{
				Type:     convertMountType(mnt.Type),
				Source:   mountSource(mnt),
				Target:   "/volumes/" + modelName + mountDest,
				ReadOnly: true,
			})
		}

		return "/volumes/" + modelName + path, nil
	}

	transformedIncludes := make([]string, 0, len(includes))
	for _, p := range includes {
		tp, err := transform(p)
		if err != nil {
			log.Printf("[volumes] model %q: skipping archive include %q: %v", modelName, p, err)
			continue
		}
		transformedIncludes = append(transformedIncludes, tp)
	}

	// Excludes are glob patterns that apply within the archive root, not paths
	// that need mount resolution. They are passed through as-is.
	transformedExcludes := make([]string, len(excludes))
	copy(transformedExcludes, excludes)

	return ArchiveVolumes{
		ModelName: modelName,
		Mounts:    mounts,
		Includes:  transformedIncludes,
		Excludes:  transformedExcludes,
	}, nil
}

func buildDestMap(mounts []container.MountPoint) map[string]container.MountPoint {
	m := make(map[string]container.MountPoint, len(mounts))
	for _, mp := range mounts {
		m[strings.TrimRight(mp.Destination, "/")] = mp
	}
	return m
}

func findMountForPath(path string, mounts map[string]container.MountPoint) (container.MountPoint, string, error) {
	path = strings.TrimRight(path, "/")

	if mp, ok := mounts[path]; ok {
		return mp, path, nil
	}

	var best container.MountPoint
	var bestDst string
	bestLen := 0
	for dst, mp := range mounts {
		if strings.HasPrefix(path, dst+"/") && len(dst) > bestLen {
			best = mp
			bestDst = dst
			bestLen = len(dst)
		}
	}
	if bestLen > 0 {
		return best, bestDst, nil
	}

	return container.MountPoint{}, "", fmt.Errorf("path %q does not match any mount", path)
}

func mountSource(mp container.MountPoint) string {
	if mp.Name != "" {
		return mp.Name
	}
	return mp.Source
}

func convertMountType(t mount.Type) mount.Type {
	switch t {
	case mount.TypeBind, mount.TypeTmpfs:
		return t
	default:
		return mount.TypeVolume
	}
}

// applyArchiveVolumes updates a model's archive.includes and archive.excludes
// with the transformed paths, and returns the volume mounts for the gobackup
// container. Duplicate mounts across models are collapsed by mount key.
func applyArchiveVolumes(models map[string]any, volumes []ArchiveVolumes) []docker.MountDef {
	dedup := map[string]docker.MountDef{}
	order := []string{}

	for _, av := range volumes {
		if len(av.Includes) == 0 && len(av.Excludes) == 0 {
			continue
		}
		model, ok := models[av.ModelName].(map[string]any)
		if !ok {
			continue
		}
		archive, _ := model["archive"].(map[string]any)
		if archive == nil {
			archive = map[string]any{}
			model["archive"] = archive
		}
		if len(av.Includes) > 0 {
			archive["includes"] = av.Includes
		}
		if len(av.Excludes) > 0 {
			archive["excludes"] = av.Excludes
		}

		// Dedup mounts by (source:target) — but with read-only, same source
		// can target different paths. Dedup by source+target.
		for _, m := range av.Mounts {
			key := m.Source + ":" + m.Target
			if _, exists := dedup[key]; !exists {
				dedup[key] = m
				order = append(order, key)
			}
		}
	}

	out := make([]docker.MountDef, 0, len(order))
	for _, k := range order {
		out = append(out, dedup[k])
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
