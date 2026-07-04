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
		md, tp, err := resolvePathMount(destMap, modelName, path, true) // archive mounts are read-only
		if err != nil {
			return "", err
		}
		if !seen[md.Target] {
			seen[md.Target] = true
			mounts = append(mounts, md)
		}
		return tp, nil
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

// resolvePathMount finds the mount backing `path` in a container and returns a
// MountDef that re-mounts that volume into the gobackup engine under
// /volumes/<modelName><mountDest>, plus `path` rewritten to that location.
func resolvePathMount(destMap map[string]container.MountPoint, modelName, path string, readOnly bool) (docker.MountDef, string, error) {
	mnt, mountDest, err := findMountForPath(path, destMap)
	if err != nil {
		return docker.MountDef{}, "", err
	}
	md := docker.MountDef{
		Type:     convertMountType(mnt.Type),
		Source:   mountSource(mnt),
		Target:   "/volumes/" + modelName + mountDest,
		ReadOnly: readOnly,
	}
	return md, "/volumes/" + modelName + path, nil
}

// sqliteMountsForModel rewrites each sqlite database's `path` to where its
// backing volume is mounted in the engine, and returns those mounts. The mounts
// are READ-WRITE: gobackup dumps sqlite via `sqlite3 <path> .dump`, which opens
// the DB and (for WAL-mode databases) needs to write -wal/-shm sidecar files.
// A path not backed by a mount, absent, or already transformed is skipped.
func sqliteMountsForModel(destMap map[string]container.MountPoint, modelName string, dbs map[string]any) []docker.MountDef {
	var mounts []docker.MountDef
	seen := map[string]bool{}
	for id, dbv := range dbs {
		db, ok := dbv.(map[string]any)
		if !ok || db["type"] != "sqlite" {
			continue
		}
		path, _ := db["path"].(string)
		if path == "" || strings.HasPrefix(path, "/volumes/") {
			continue
		}
		md, tp, err := resolvePathMount(destMap, modelName, path, false) // RW — see doc
		if err != nil {
			log.Printf("[volumes] model %q sqlite %q: path %q not on a mounted volume; skipping (backup will fail unless the file is already in the engine)", modelName, id, path)
			continue
		}
		db["path"] = tp
		if !seen[md.Target] {
			seen[md.Target] = true
			mounts = append(mounts, md)
		}
	}
	return mounts
}

// dedupMountsByTarget collapses mounts sharing a target (a container can't have
// two mounts at one path). When both a read-only and a read-write mount want the
// same target, the writable one wins — it is a superset (sqlite needs RW; an
// archive read is fine on a RW mount).
func dedupMountsByTarget(mounts []docker.MountDef) []docker.MountDef {
	idx := map[string]int{}
	var out []docker.MountDef
	for _, m := range mounts {
		if i, ok := idx[m.Target]; ok {
			if out[i].ReadOnly && !m.ReadOnly {
				out[i] = m // read-write wins
			}
			continue
		}
		idx[m.Target] = len(out)
		out = append(out, m)
	}
	return out
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
