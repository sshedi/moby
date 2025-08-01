package worker

import (
	"context"
	"fmt"
	"io"
	nethttp "net/http"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/gc"
	"github.com/containerd/containerd/v2/pkg/rootfs"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/moby/buildkit/cache"
	cacheconfig "github.com/moby/buildkit/cache/config"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/exporter"
	localexporter "github.com/moby/buildkit/exporter/local"
	tarexporter "github.com/moby/buildkit/exporter/tar"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	containerdsnapshot "github.com/moby/buildkit/snapshot/containerd"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver/cdidevices"
	"github.com/moby/buildkit/solver/llbsolver/mounts"
	"github.com/moby/buildkit/solver/llbsolver/ops"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/source"
	"github.com/moby/buildkit/source/containerimage"
	"github.com/moby/buildkit/source/git"
	"github.com/moby/buildkit/source/http"
	"github.com/moby/buildkit/source/local"
	"github.com/moby/buildkit/util/archutil"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/moby/buildkit/util/leaseutil"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/version"
	pkgprogress "github.com/moby/moby/api/pkg/progress"
	imageadapter "github.com/moby/moby/v2/daemon/internal/builder-next/adapters/containerimage"
	mobyexporter "github.com/moby/moby/v2/daemon/internal/builder-next/exporter"
	"github.com/moby/moby/v2/daemon/internal/builder-next/worker/mod"
	distmetadata "github.com/moby/moby/v2/daemon/internal/distribution/metadata"
	"github.com/moby/moby/v2/daemon/internal/distribution/xfer"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/semaphore"
)

func init() {
	if v := mod.Version("github.com/moby/buildkit"); v != "" {
		version.Version = v
	}
}

const labelCreatedAt = "buildkit/createdat"

// LayerAccess provides access to a moby layer from a snapshot
type LayerAccess interface {
	GetDiffIDs(ctx context.Context, key string) ([]layer.DiffID, error)
	EnsureLayer(ctx context.Context, key string) ([]layer.DiffID, error)
}

// Opt defines a structure for creating a worker.
type Opt struct {
	ID                string
	Labels            map[string]string
	GCPolicy          []client.PruneInfo
	Executor          executor.Executor
	Snapshotter       snapshot.Snapshotter
	ContentStore      *containerdsnapshot.Store
	CacheManager      cache.Manager
	LeaseManager      *leaseutil.Manager
	GarbageCollect    func(context.Context) (gc.Stats, error)
	ImageSource       *imageadapter.Source
	DownloadManager   *xfer.LayerDownloadManager
	V2MetadataService distmetadata.V2MetadataService
	Transport         nethttp.RoundTripper
	Exporter          exporter.Exporter
	Layers            LayerAccess
	Platforms         []ocispec.Platform
	CDIManager        *cdidevices.Manager
}

// Worker is a local worker instance with dedicated snapshotter, cache, and so on.
// TODO: s/Worker/OpWorker/g ?
type Worker struct {
	Opt
	SourceManager *source.Manager
}

var _ interface {
	GetRemotes(context.Context, cache.ImmutableRef, bool, cacheconfig.RefConfig, bool, session.Group) ([]*solver.Remote, error)
} = &Worker{}

// NewWorker instantiates a local worker
func NewWorker(opt Opt) (*Worker, error) {
	sm, err := source.NewManager()
	if err != nil {
		return nil, err
	}

	cm := opt.CacheManager
	sm.Register(opt.ImageSource)

	gs, err := git.NewSource(git.Opt{
		CacheAccessor: cm,
	})
	if err == nil {
		sm.Register(gs)
	} else {
		log.G(context.TODO()).Warnf("Could not register builder git source: %s", err)
	}

	hs, err := http.NewSource(http.Opt{
		CacheAccessor: cm,
		Transport:     opt.Transport,
	})
	if err == nil {
		sm.Register(hs)
	} else {
		log.G(context.TODO()).Warnf("Could not register builder http source: %s", err)
	}

	ss, err := local.NewSource(local.Opt{
		CacheAccessor: cm,
	})
	if err == nil {
		sm.Register(ss)
	} else {
		log.G(context.TODO()).Warnf("Could not register builder local source: %s", err)
	}

	return &Worker{
		Opt:           opt,
		SourceManager: sm,
	}, nil
}

// ID returns worker ID
func (w *Worker) ID() string {
	return w.Opt.ID
}

// Labels returns map of all worker labels
func (w *Worker) Labels() map[string]string {
	return w.Opt.Labels
}

// Platforms returns one or more platforms supported by the image.
func (w *Worker) Platforms(noCache bool) []ocispec.Platform {
	if noCache {
		w.Opt.Platforms = mergePlatforms(w.Opt.Platforms, archutil.SupportedPlatforms(noCache))
	}
	if len(w.Opt.Platforms) == 0 {
		return []ocispec.Platform{platforms.DefaultSpec()}
	}
	return w.Opt.Platforms
}

// mergePlatforms merges the defined platforms with the supported platforms
// and returns a new slice of platforms. It ensures no duplicates.
func mergePlatforms(defined, supported []ocispec.Platform) []ocispec.Platform {
	result := []ocispec.Platform{}
	matchers := make([]platforms.MatchComparer, len(defined))
	for i, p := range defined {
		result = append(result, p)
		matchers[i] = platforms.Only(p)
	}
	for _, p := range supported {
		exists := false
		for _, m := range matchers {
			if m.Match(p) {
				exists = true
				break
			}
		}
		if !exists {
			result = append(result, p)
		}
	}
	return result
}

// GCPolicy returns automatic GC Policy
func (w *Worker) GCPolicy() []client.PruneInfo {
	return w.Opt.GCPolicy
}

// BuildkitVersion returns BuildKit version
func (w *Worker) BuildkitVersion() client.BuildkitVersion {
	return client.BuildkitVersion{
		Package:  version.Package,
		Version:  version.Version + "-moby",
		Revision: version.Revision,
	}
}

func (w *Worker) GarbageCollect(ctx context.Context) error {
	if w.Opt.GarbageCollect == nil {
		return nil
	}
	_, err := w.Opt.GarbageCollect(ctx)
	return err
}

// Close closes the worker and releases all resources
func (w *Worker) Close() error {
	return nil
}

// ContentStore returns the wrapped content store
func (w *Worker) ContentStore() *containerdsnapshot.Store {
	return w.Opt.ContentStore
}

// LeaseManager returns the wrapped lease manager
func (w *Worker) LeaseManager() *leaseutil.Manager {
	return w.Opt.LeaseManager
}

// LoadRef loads a reference by ID
func (w *Worker) LoadRef(ctx context.Context, id string, hidden bool) (cache.ImmutableRef, error) {
	var opts []cache.RefOption
	if hidden {
		opts = append(opts, cache.NoUpdateLastUsed)
	}
	if id == "" {
		// results can have nil refs if they are optimized out to be equal to scratch,
		// i.e. Diff(A,A) == scratch
		return nil, nil
	}

	return w.CacheManager().Get(ctx, id, nil, opts...)
}

func (w *Worker) ResolveSourceMetadata(ctx context.Context, op *pb.SourceOp, opt sourceresolver.Opt, sm *session.Manager, g session.Group) (*sourceresolver.MetaResponse, error) {
	if opt.SourcePolicies != nil {
		return nil, errors.New("source policies can not be set for worker")
	}

	var platform *pb.Platform
	if p := opt.Platform; p != nil {
		platform = &pb.Platform{
			Architecture: p.Architecture,
			OS:           p.OS,
			Variant:      p.Variant,
			OSVersion:    p.OSVersion,
		}
	}

	id, err := w.SourceManager.Identifier(&pb.Op_Source{Source: op}, platform)
	if err != nil {
		return nil, err
	}

	switch idt := id.(type) {
	case *containerimage.ImageIdentifier:
		if opt.ImageOpt == nil {
			opt.ImageOpt = &sourceresolver.ResolveImageOpt{}
		}
		dgst, config, err := w.ImageSource.ResolveImageConfig(ctx, idt.Reference.String(), opt, sm, g)
		if err != nil {
			return nil, err
		}
		return &sourceresolver.MetaResponse{
			Op: op,
			Image: &sourceresolver.ResolveImageResponse{
				Digest: dgst,
				Config: config,
			},
		}, nil
	}

	return &sourceresolver.MetaResponse{
		Op: op,
	}, nil
}

// ResolveOp converts a LLB vertex into a LLB operation
func (w *Worker) ResolveOp(v solver.Vertex, s frontend.FrontendLLBBridge, sm *session.Manager) (solver.Op, error) {
	if baseOp, ok := v.Sys().(*pb.Op); ok {
		// TODO do we need to pass a value here? Where should it come from? https://github.com/moby/buildkit/commit/b3cf7c43cfefdfd7a945002c0e76b54e346ab6cf
		var parallelism *semaphore.Weighted
		switch op := baseOp.Op.(type) {
		case *pb.Op_Source:
			return ops.NewSourceOp(v, op, baseOp.Platform, w.SourceManager, parallelism, sm, w)
		case *pb.Op_Exec:
			return ops.NewExecOp(v, op, baseOp.Platform, w.CacheManager(), parallelism, sm, w.Executor(), w)
		case *pb.Op_File:
			return ops.NewFileOp(v, op, w.CacheManager(), parallelism, w)
		case *pb.Op_Build:
			return ops.NewBuildOp(v, op, s, w)
		case *pb.Op_Merge:
			return ops.NewMergeOp(v, op, w)
		case *pb.Op_Diff:
			return ops.NewDiffOp(v, op, w)
		}
	}
	return nil, errors.Errorf("could not resolve %v", v)
}

// ResolveImageConfig returns image config for an image
func (w *Worker) ResolveImageConfig(ctx context.Context, ref string, opt sourceresolver.Opt, sm *session.Manager, g session.Group) (digest.Digest, []byte, error) {
	return w.ImageSource.ResolveImageConfig(ctx, ref, opt, sm, g)
}

// DiskUsage returns disk usage report
func (w *Worker) DiskUsage(ctx context.Context, opt client.DiskUsageInfo) ([]*client.UsageInfo, error) {
	return w.CacheManager().DiskUsage(ctx, opt)
}

// Prune deletes reclaimable build cache
func (w *Worker) Prune(ctx context.Context, ch chan client.UsageInfo, info ...client.PruneInfo) error {
	return w.CacheManager().Prune(ctx, ch, info...)
}

// Exporter returns exporter by name
func (w *Worker) Exporter(name string, sm *session.Manager) (exporter.Exporter, error) {
	switch name {
	case mobyexporter.Moby:
		return w.Opt.Exporter, nil
	case client.ExporterLocal:
		return localexporter.New(localexporter.Opt{
			SessionManager: sm,
		})
	case client.ExporterTar:
		return tarexporter.New(tarexporter.Opt{
			SessionManager: sm,
		})
	default:
		return nil, errors.Errorf("exporter %q could not be found", name)
	}
}

// GetRemotes returns the remote snapshot references given a local reference
func (w *Worker) GetRemotes(ctx context.Context, ref cache.ImmutableRef, createIfNeeded bool, _ cacheconfig.RefConfig, all bool, s session.Group) ([]*solver.Remote, error) {
	if ref == nil {
		return nil, nil
	}
	var diffIDs []layer.DiffID
	var err error
	if !createIfNeeded {
		diffIDs, err = w.Layers.GetDiffIDs(ctx, ref.ID())
		if err != nil {
			return nil, err
		}
	} else {
		if err := ref.Finalize(ctx); err != nil {
			return nil, err
		}
		if err := ref.Extract(ctx, s); err != nil {
			return nil, err
		}
		diffIDs, err = w.Layers.EnsureLayer(ctx, ref.ID())
		if err != nil {
			return nil, err
		}
	}

	descriptors := make([]ocispec.Descriptor, len(diffIDs))
	for i, dgst := range diffIDs {
		descriptors[i] = ocispec.Descriptor{
			MediaType: c8dimages.MediaTypeDockerSchema2Layer,
			Digest:    dgst,
			Size:      -1,
		}
	}

	return []*solver.Remote{{
		Descriptors: descriptors,
		Provider:    &emptyProvider{},
	}}, nil
}

// PruneCacheMounts removes the current cache snapshots for specified IDs
func (w *Worker) PruneCacheMounts(ctx context.Context, ids map[string]bool) error {
	mu := mounts.CacheMountsLocker()
	mu.Lock()
	defer mu.Unlock()

	for id, nested := range ids {
		mds, err := mounts.SearchCacheDir(ctx, w.CacheManager(), id, nested)
		if err != nil {
			return err
		}
		for _, md := range mds {
			if err := md.SetCachePolicyDefault(); err != nil {
				return err
			}
			if err := md.ClearCacheDirIndex(); err != nil {
				return err
			}
			// if ref is unused try to clean it up right away by releasing it
			if mref, err := w.CacheManager().GetMutable(ctx, md.ID()); err == nil {
				go mref.Release(context.TODO())
			}
		}
	}

	mounts.ClearActiveCacheMounts()
	return nil
}

func (w *Worker) getRef(ctx context.Context, diffIDs []layer.DiffID, opts ...cache.RefOption) (cache.ImmutableRef, error) {
	var parent cache.ImmutableRef
	if len(diffIDs) > 1 {
		var err error
		parent, err = w.getRef(ctx, diffIDs[:len(diffIDs)-1], opts...)
		if err != nil {
			return nil, err
		}
		defer parent.Release(context.TODO())
	}
	return w.CacheManager().GetByBlob(context.TODO(), ocispec.Descriptor{
		Annotations: map[string]string{
			"containerd.io/uncompressed": diffIDs[len(diffIDs)-1].String(),
		},
	}, parent, opts...)
}

// FromRemote converts a remote snapshot reference to a local one
func (w *Worker) FromRemote(ctx context.Context, remote *solver.Remote) (cache.ImmutableRef, error) {
	rootfs, err := getLayers(ctx, remote.Descriptors)
	if err != nil {
		return nil, err
	}

	layers := make([]xfer.DownloadDescriptor, 0, len(rootfs))

	for _, l := range rootfs {
		// ongoing.add(desc)
		layers = append(layers, &layerDescriptor{
			desc:     l.Blob,
			diffID:   l.Diff.Digest,
			provider: remote.Provider,
			w:        w,
			pctx:     ctx,
		})
	}

	defer func() {
		for _, l := range rootfs {
			w.ContentStore().Delete(context.TODO(), l.Blob.Digest)
		}
	}()

	rootFS, release, err := w.DownloadManager.Download(ctx, layers, &discardProgress{})
	if err != nil {
		return nil, err
	}
	defer release()

	if len(rootFS.DiffIDs) != len(layers) {
		return nil, errors.Errorf("invalid layer count mismatch %d vs %d", len(rootFS.DiffIDs), len(layers))
	}

	for i := range rootFS.DiffIDs {
		tm := time.Now()
		if tmstr, ok := remote.Descriptors[i].Annotations[labelCreatedAt]; ok {
			if err := (&tm).UnmarshalText([]byte(tmstr)); err != nil {
				return nil, err
			}
		}
		descr := fmt.Sprintf("imported %s", remote.Descriptors[i].Digest)
		if v, ok := remote.Descriptors[i].Annotations["buildkit/description"]; ok {
			descr = v
		}
		ref, err := w.getRef(ctx, rootFS.DiffIDs[:i+1], cache.WithDescription(descr), cache.WithCreationTime(tm))
		if err != nil {
			return nil, err
		}
		if i == len(remote.Descriptors)-1 {
			return ref, nil
		}
		defer ref.Release(context.TODO())
	}

	return nil, errors.Errorf("unreachable")
}

// Executor returns executor.Executor for running processes
func (w *Worker) Executor() executor.Executor {
	return w.Opt.Executor
}

// CacheManager returns cache.Manager for accessing local storage
func (w *Worker) CacheManager() cache.Manager {
	return w.Opt.CacheManager
}

func (w *Worker) CDIManager() *cdidevices.Manager {
	return w.Opt.CDIManager
}

type discardProgress struct{}

func (*discardProgress) WriteProgress(_ pkgprogress.Progress) error {
	return nil
}

// Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error)
type layerDescriptor struct {
	provider content.Provider
	desc     ocispec.Descriptor
	diffID   layer.DiffID
	// ref      ctdreference.Spec
	w    *Worker
	pctx context.Context
}

func (ld *layerDescriptor) Key() string {
	return "v2:" + ld.desc.Digest.String()
}

func (ld *layerDescriptor) ID() string {
	return ld.desc.Digest.String()
}

func (ld *layerDescriptor) DiffID() (layer.DiffID, error) {
	return ld.diffID, nil
}

func (ld *layerDescriptor) Download(ctx context.Context, progressOutput pkgprogress.Output) (io.ReadCloser, int64, error) {
	done := oneOffProgress(ld.pctx, fmt.Sprintf("pulling %s", ld.desc.Digest))

	// TODO should this write output to progressOutput? Or use something similar to loggerFromContext()? see https://github.com/moby/buildkit/commit/aa29e7729464f3c2a773e27795e584023c751cb8
	discardLogs := func(_ []byte) {}
	if err := contentutil.Copy(ctx, ld.w.ContentStore(), ld.provider, ld.desc, "", discardLogs); err != nil {
		return nil, 0, done(err)
	}
	_ = done(nil)

	ra, err := ld.w.ContentStore().ReaderAt(ctx, ld.desc)
	if err != nil {
		return nil, 0, err
	}

	return io.NopCloser(content.NewReader(ra)), ld.desc.Size, nil
}

func (ld *layerDescriptor) Close() {
	// ld.is.ContentStore().Delete(context.TODO(), ld.desc.Digest)
}

func (ld *layerDescriptor) Registered(diffID layer.DiffID) {
	// Cache mapping from this layer's DiffID to the blobsum
	ld.w.V2MetadataService.Add(diffID, distmetadata.V2Metadata{Digest: ld.desc.Digest})
}

func getLayers(ctx context.Context, descs []ocispec.Descriptor) ([]rootfs.Layer, error) {
	layers := make([]rootfs.Layer, len(descs))
	for i, desc := range descs {
		diffIDStr := desc.Annotations["containerd.io/uncompressed"]
		if diffIDStr == "" {
			return nil, errors.Errorf("%s missing uncompressed digest", desc.Digest)
		}
		diffID, err := digest.Parse(diffIDStr)
		if err != nil {
			return nil, err
		}
		layers[i].Diff = ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageLayer,
			Digest:    diffID,
		}
		layers[i].Blob = ocispec.Descriptor{
			MediaType: desc.MediaType,
			Digest:    desc.Digest,
			Size:      desc.Size,
		}
	}
	return layers, nil
}

func oneOffProgress(ctx context.Context, id string) func(err error) error {
	pw, _, _ := progress.NewFromContext(ctx)
	s := time.Now()
	st := progress.Status{
		Started: &s,
	}
	_ = pw.Write(id, st)
	return func(err error) error {
		// TODO: set error on status
		c := time.Now()
		st.Completed = &c
		_ = pw.Write(id, st)
		_ = pw.Close()
		return err
	}
}

type emptyProvider struct{}

func (p *emptyProvider) ReaderAt(ctx context.Context, dec ocispec.Descriptor) (content.ReaderAt, error) {
	return nil, errors.Errorf("ReaderAt not implemented for empty provider")
}

func (p *emptyProvider) Info(ctx context.Context, d digest.Digest) (content.Info, error) {
	return content.Info{}, errors.Wrapf(cerrdefs.ErrNotImplemented, "Info not implemented for empty provider")
}
