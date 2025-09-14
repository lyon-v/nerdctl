package commitctr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/rootfs"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Options defines minimal options to commit a container into a new image.
type Options struct {
	// TargetRef is the repository[:tag] of the new image.
	TargetRef string
	// Author of the new image.
	Author string
	// Message is the commit message (recorded in History.Comment).
	Message string
	// Pause indicates whether to pause the container during commit.
	Pause bool
	// ChangeCMD overwrites image config CMD when non-nil.
	ChangeCMD []string
	// ChangeEntrypoint overwrites image config Entrypoint when non-nil.
	ChangeEntrypoint []string
	// Format chooses media types for config/manifest/layers: "oci" or "docker". Defaults to "docker".
	Format string
}

// Commit creates a new image from a container's filesystem changes and returns the config digest.
// This implementation only depends on github.com/containerd/containerd and standard library packages.
func Commit(ctx context.Context, client *containerd.Client, containerID string, opts Options) (digest.Digest, error) {
	if opts.TargetRef == "" {
		return "", errors.New("TargetRef must be specified")
	}

	ctr, err := client.LoadContainer(ctx, containerID)
	if err != nil {
		return "", err
	}

	info, err := ctr.Info(ctx)
	if err != nil {
		return "", err
	}
	// Ensure the container task is running; otherwise do not commit
	task, err := ctr.Task(ctx, cio.Load)
	if err != nil {
		return "", errors.New("task must be running")
	}
	st, err := task.Status(ctx)
	if err != nil {
		return "", err
	}
	if st.Status != containerd.Running {
		return "", errors.New("task must be running")
	}

	// Optionally pause the container task if running
	if opts.Pause {
		if err := pauseIfRunning(ctx, ctr); err != nil {
			return "", err
		}
		defer resumeIfPaused(ctx, ctr)
	}

	// Acquire a short-lived lease to protect temporary content from GC during commit
	ctx, done, err := client.WithLease(ctx, leases.WithRandomID(), leases.WithExpiration(1*time.Hour))
	if err != nil {
		return "", fmt.Errorf("failed to create lease for commit: %w", err)
	}
	defer done(ctx)

	// Resolve base image and read its config
	imgSvc := client.ImageService()
	baseImgRec, err := imgSvc.Get(ctx, info.Image)
	if err != nil {
		return "", fmt.Errorf("container %q lacks image: %w", containerID, err)
	}
	baseImg := containerd.NewImage(client, baseImgRec)

	baseConfig, baseManifest, err := readBaseConfigAndManifest(ctx, baseImg)
	if err != nil {
		return "", fmt.Errorf("failed to read base image config/manifest: %w", err)
	}

	// Create a filesystem diff layer from the container snapshot
	cs := client.ContentStore()
	differ := client.DiffService()
	sn := client.SnapshotService(info.Snapshotter)

	layerMediaType := selectLayerMediaType(opts.Format)
	diffDesc, diffID, err := createDiff(ctx, containerID, sn, cs, differ, layerMediaType)
	if err != nil {
		return "", fmt.Errorf("failed to create diff layer: %w", err)
	}

	// Build new image config by applying changes and appending new diffID
	newConfig := buildNewImageConfig(*baseConfig.Config, diffID, opts)

	// Write new config and manifest into content store and create/update the image
	configMediaType, manifestMediaType := selectConfigAndManifestMediaTypes(opts.Format)
	manifestDesc, configDigest, err := writeContentsForImage(ctx, cs, baseManifest, newConfig, diffDesc, configMediaType, manifestMediaType)
	if err != nil {
		return "", err
	}

	img := images.Image{
		Name:      opts.TargetRef,
		Target:    manifestDesc,
		CreatedAt: time.Now(),
	}
	if _, err := imgSvc.Update(ctx, img); err != nil {
		// If update fails (e.g. not found), try create
		if _, err := imgSvc.Create(ctx, img); err != nil {
			return "", fmt.Errorf("failed to create or update image %s: %w", opts.TargetRef, err)
		}
	}

	// Unpack the image into the same snapshotter for immediate usability
	cimg := containerd.NewImage(client, img)
	if err := cimg.Unpack(ctx, info.Snapshotter); err != nil {
		return "", err
	}

	return configDigest, nil
}

func pauseIfRunning(ctx context.Context, ctr containerd.Container) error {
	task, err := ctr.Task(ctx, cio.Load)
	if err != nil {
		// No task, nothing to pause
		return nil
	}
	st, err := task.Status(ctx)
	if err != nil {
		return err
	}
	switch st.Status {
	case containerd.Running:
		return task.Pause(ctx)
	default:
		return nil
	}
}

func resumeIfPaused(ctx context.Context, ctr containerd.Container) {
	task, err := ctr.Task(ctx, cio.Load)
	if err != nil {
		return
	}
	st, err := task.Status(ctx)
	if err != nil {
		return
	}
	if st.Status == containerd.Paused {
		_ = task.Resume(ctx)
	}
}

func selectLayerMediaType(format string) string {
	if format == "oci" {
		return ocispec.MediaTypeImageLayerGzip
	}
	// default docker
	return images.MediaTypeDockerSchema2LayerGzip
}

func selectConfigAndManifestMediaTypes(format string) (string, string) {
	if format == "oci" {
		return ocispec.MediaTypeImageConfig, ocispec.MediaTypeImageManifest
	}
	return images.MediaTypeDockerSchema2Config, images.MediaTypeDockerSchema2Manifest
}

func createDiff(ctx context.Context, name string, sn snapshots.Snapshotter, cs content.Store, comparer diff.Comparer, layerMediaType string) (ocispec.Descriptor, digest.Digest, error) {
	newDesc, err := rootfs.CreateDiff(ctx, name, sn, comparer, diff.WithMediaType(layerMediaType))
	if err != nil {
		return ocispec.Descriptor{}, "", err
	}

	info, err := cs.Info(ctx, newDesc.Digest)
	if err != nil {
		return ocispec.Descriptor{}, "", err
	}

	diffIDStr, ok := info.Labels["containerd.io/uncompressed"]
	if !ok {
		return ocispec.Descriptor{}, "", fmt.Errorf("invalid differ response with no diffID")
	}

	diffID, err := digest.Parse(diffIDStr)
	if err != nil {
		return ocispec.Descriptor{}, "", err
	}

	return ocispec.Descriptor{
		MediaType:   layerMediaType,
		Digest:      newDesc.Digest,
		Size:        newDesc.Size,
		Annotations: map[string]string{
			// pass through annotations if needed later
		},
	}, diffID, nil
}

type baseImageInfo struct {
	Config   *ocispec.Image
	Manifest ocispec.Manifest
}

func readBaseConfigAndManifest(ctx context.Context, img containerd.Image) (baseImageInfo, ocispec.Manifest, error) {
	var bi baseImageInfo
	cs := img.ContentStore()

	// Try to read config via Image.Config (works for single-platform or platform-selected images)
	configDesc, cfgErr := img.Config(ctx)
	if cfgErr == nil {
		if cfgJSON, err := content.ReadBlob(ctx, cs, configDesc); err == nil {
			_ = json.Unmarshal(cfgJSON, &bi.Config)
		}
	}

	// Determine manifest
	target := img.Target()
	if images.IsManifestType(target.MediaType) {
		maniJSON, err := content.ReadBlob(ctx, cs, target)
		if err != nil {
			return bi, ocispec.Manifest{}, err
		}
		var manifest ocispec.Manifest
		if err := json.Unmarshal(maniJSON, &manifest); err != nil {
			return bi, ocispec.Manifest{}, err
		}
		// If config unknown yet, try read it from manifest.Config
		if bi.Config == nil {
			if cfgJSON, err := content.ReadBlob(ctx, cs, manifest.Config); err == nil {
				var imgConfig ocispec.Image
				_ = json.Unmarshal(cfgJSON, &imgConfig)
				bi.Config = &imgConfig
			}
		}
		bi.Manifest = manifest
		return bi, manifest, nil
	}

	if images.IsIndexType(target.MediaType) {
		idxJSON, err := content.ReadBlob(ctx, cs, target)
		if err != nil {
			return bi, ocispec.Manifest{}, err
		}
		var idx ocispec.Index
		if err := json.Unmarshal(idxJSON, &idx); err != nil {
			return bi, ocispec.Manifest{}, err
		}
		// If we have configDesc from img.Config, try to match manifest by config
		if cfgErr == nil {
			for _, mDesc := range idx.Manifests {
				if maniJSON, err := content.ReadBlob(ctx, cs, mDesc); err == nil {
					var m ocispec.Manifest
					if json.Unmarshal(maniJSON, &m) == nil && reflect.DeepEqual(m.Config, configDesc) {
						// fill config if missing
						if bi.Config == nil {
							if cfgJSON, err := content.ReadBlob(ctx, cs, m.Config); err == nil {
								var imgConfig ocispec.Image
								_ = json.Unmarshal(cfgJSON, &imgConfig)
								bi.Config = &imgConfig
							}
						}
						bi.Manifest = m
						return bi, m, nil
					}
				}
			}
		}
		// Fallback: pick the first manifest
		if len(idx.Manifests) == 0 {
			return bi, ocispec.Manifest{}, fmt.Errorf("base image index has no manifests")
		}
		first := idx.Manifests[0]
		maniJSON, err := content.ReadBlob(ctx, cs, first)
		if err != nil {
			return bi, ocispec.Manifest{}, err
		}
		var manifest ocispec.Manifest
		if err := json.Unmarshal(maniJSON, &manifest); err != nil {
			return bi, ocispec.Manifest{}, err
		}
		// Fill config if missing
		if bi.Config == nil {
			if cfgJSON, err := content.ReadBlob(ctx, cs, manifest.Config); err == nil {
				var imgConfig ocispec.Image
				_ = json.Unmarshal(cfgJSON, &imgConfig)
				bi.Config = &imgConfig
			}
		}
		bi.Manifest = manifest
		return bi, manifest, nil
	}

	return bi, ocispec.Manifest{}, fmt.Errorf("unsupported base image media type: %s", target.MediaType)
}

func buildNewImageConfig(base ocispec.Image, diffID digest.Digest, opts Options) ocispec.Image {
	created := time.Now()
	if opts.Author != "" {
		base.Author = opts.Author
	}
	// Apply changes
	if opts.ChangeCMD != nil {
		base.Config.Cmd = opts.ChangeCMD
	}
	if opts.ChangeEntrypoint != nil {
		base.Config.Entrypoint = opts.ChangeEntrypoint
	}
	// Append new diffID
	base.RootFS.DiffIDs = append(base.RootFS.DiffIDs, diffID)
	base.RootFS.Type = "layers"
	// Append history entry
	base.History = append(base.History, ocispec.History{
		Created:    &created,
		Author:     base.Author,
		Comment:    opts.Message,
		EmptyLayer: false,
	})
	if base.Created == nil {
		base.Created = &created
	}
	return base
}

func writeContentsForImage(
	ctx context.Context,
	cs content.Store,
	baseManifest ocispec.Manifest,
	newConfig ocispec.Image,
	diffLayerDesc ocispec.Descriptor,
	configMediaType, manifestMediaType string,
) (ocispec.Descriptor, digest.Digest, error) {
	// Marshal new config
	configJSON, err := json.Marshal(newConfig)
	if err != nil {
		return ocispec.Descriptor{}, "", err
	}
	configDesc := ocispec.Descriptor{
		MediaType: configMediaType,
		Digest:    digest.FromBytes(configJSON),
		Size:      int64(len(configJSON)),
	}

	// Build new manifest inheriting base layers and appending new layer
	layers := append(baseManifest.Layers, diffLayerDesc)
	newManifest := struct {
		MediaType string `json:"mediaType,omitempty"`
		ocispec.Manifest
	}{
		MediaType: manifestMediaType,
		Manifest: ocispec.Manifest{
			Versioned: specs.Versioned{SchemaVersion: 2},
			Config:    configDesc,
			Layers:    layers,
		},
	}

	manifestJSON, err := json.MarshalIndent(newManifest, "", "    ")
	if err != nil {
		return ocispec.Descriptor{}, "", err
	}
	manifestDesc := ocispec.Descriptor{
		MediaType: manifestMediaType,
		Digest:    digest.FromBytes(manifestJSON),
		Size:      int64(len(manifestJSON)),
	}

	// Link blobs with GC reference labels
	labels := map[string]string{
		"containerd.io/gc.ref.content.0": configDesc.Digest.String(),
	}
	for i, l := range layers {
		labels[fmt.Sprintf("containerd.io/gc.ref.content.%d", i+1)] = l.Digest.String()
	}

	if err := content.WriteBlob(ctx, cs, manifestDesc.Digest.String(), bytes.NewReader(manifestJSON), manifestDesc, content.WithLabels(labels)); err != nil {
		return ocispec.Descriptor{}, "", err
	}
	if err := content.WriteBlob(ctx, cs, configDesc.Digest.String(), bytes.NewReader(configJSON), configDesc); err != nil {
		return ocispec.Descriptor{}, "", err
	}

	return manifestDesc, configDesc.Digest, nil
}
