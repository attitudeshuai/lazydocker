package commands

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// ObjectType identifies the kind of a docker object inside the relation graph.
type ObjectType string

const (
	// ContainerObject is a docker container
	ContainerObject ObjectType = "container"
	// ImageObject is a docker image
	ImageObject ObjectType = "image"
	// VolumeObject is a docker volume
	VolumeObject ObjectType = "volume"
	// NetworkObject is a docker network
	NetworkObject ObjectType = "network"
	// ServiceObject is a compose service (project + service name)
	ServiceObject ObjectType = "service"
	// ProjectObject is a compose project
	ProjectObject ObjectType = "project"
)

// ReferenceKind classifies why an object references another one.
type ReferenceKind string

const (
	// ImageReference: container uses an image
	ImageReference ReferenceKind = "image"
	// VolumeReference: container mounts a volume
	VolumeReference ReferenceKind = "volume"
	// NetworkReference: container is connected to a network
	NetworkReference ReferenceKind = "network"
)

// ObjectRef points at a single object inside the relation graph.
type ObjectRef struct {
	Type ObjectType
	// ID is the canonical identifier used by the docker API for the object.
	// For volumes and networks this is the name (that is what the API removes by);
	// for containers and images it is the sha id; for services it is
	// "<project>-<service>"; for projects it is the project name.
	ID string
	// Name is a human readable label for the object
	Name string
}

func (r ObjectRef) String() string {
	if r.Name != "" && r.Name != r.ID {
		return fmt.Sprintf("%s %s (%s)", r.Type, r.Name, r.ID)
	}
	return fmt.Sprintf("%s %s", r.Type, r.ID)
}

// Reference is a single directed edge: Referrer uses Target.
// Project and Service attribute the edge to the compose project/service the
// referrer belongs to (both are empty for standalone containers).
type Reference struct {
	Referrer ObjectRef
	Target   ObjectRef
	Kind     ReferenceKind
	Project  string
	Service  string
	// Detail carries extra context, e.g. the mount destination or the
	// IP address on a network
	Detail string
}

// Uncertainty reports that the references of an object (or one kind of them)
// could not be determined. An uncertainty is never the same as "no references".
type Uncertainty struct {
	Object ObjectRef
	// Kind is the reference kind that could not be resolved, or empty when
	// the whole object could not be resolved.
	Kind   ReferenceKind
	Reason string
}

// nodeKey is the identity of an object independent of its display name:
// reverse lookups (which may be made with an empty Name) must resolve to the
// same node.
type nodeKey struct {
	Type ObjectType
	ID   string
}

func nodeKeyOf(ref ObjectRef) nodeKey {
	return nodeKey{Type: ref.Type, ID: ref.ID}
}

type uncertaintyKey struct {
	node nodeKey
	kind ReferenceKind
}

// RelationGraph is an immutable snapshot of the cross-object references at the
// time it was built. New graphs are built off to the side and atomically
// swapped in, so readers never observe a half-updated graph.
type RelationGraph struct {
	BuiltAt time.Time

	nodes    map[nodeKey]bool
	outgoing map[nodeKey][]Reference
	incoming map[nodeKey][]Reference

	// serviceContainers / projectContainers allow forward queries from a
	// service or project node (union of its containers' edges).
	serviceContainers map[string][]ObjectRef
	projectContainers map[string][]ObjectRef

	unknown map[uncertaintyKey]Uncertainty
}

// Ready reports whether a graph has ever been built. An unbuilt graph must be
// reported as "reference data unavailable", never as "no references".
func (g *RelationGraph) Ready() bool {
	return g != nil && !g.BuiltAt.IsZero()
}

// HasNode reports whether ref was observed during the last collection.
func (g *RelationGraph) HasNode(ref ObjectRef) bool {
	if g == nil {
		return false
	}
	return g.nodes[nodeKeyOf(ref)]
}

// References returns the objects used by ref (forward query). For service and
// project nodes it is the union of the edges of their containers.
func (g *RelationGraph) References(ref ObjectRef) []Reference {
	if g == nil {
		return nil
	}
	switch ref.Type {
	case ServiceObject:
		return g.unionContainerEdges(g.serviceContainers[ref.ID])
	case ProjectObject:
		return g.unionContainerEdges(g.projectContainers[ref.ID])
	default:
		return copyRefs(g.outgoing[nodeKeyOf(ref)])
	}
}

// Referrers returns the containers referencing ref (reverse query). Each edge
// is attributed to a project and service, so callers can group the answer by
// project/service or flatten it.
func (g *RelationGraph) Referrers(ref ObjectRef) []Reference {
	if g == nil {
		return nil
	}
	return copyRefs(g.incoming[nodeKeyOf(ref)])
}

func (g *RelationGraph) unionContainerEdges(containers []ObjectRef) []Reference {
	seen := make(map[nodeKey]bool)
	var result []Reference
	for _, ctr := range containers {
		for _, edge := range g.outgoing[nodeKeyOf(ctr)] {
			key := nodeKeyOf(edge.Target)
			if seen[key] {
				continue
			}
			seen[key] = true
			result = append(result, edge)
		}
	}
	return result
}

func copyRefs(in []Reference) []Reference {
	if len(in) == 0 {
		return nil
	}
	out := make([]Reference, len(in))
	copy(out, in)
	return out
}

// IsUnknown reports whether the references of the given kind for object could
// not be determined.
func (g *RelationGraph) IsUnknown(object ObjectRef, kind ReferenceKind) bool {
	if g == nil {
		return true
	}
	_, ok := g.unknown[uncertaintyKey{node: nodeKeyOf(object), kind: kind}]
	return ok
}

// Uncertainties returns all recorded uncertainties, optionally filtered to a
// single reference kind (pass "" for all).
func (g *RelationGraph) Uncertainties(kind ReferenceKind) []Uncertainty {
	if g == nil {
		return nil
	}
	result := make([]Uncertainty, 0, len(g.unknown))
	for _, u := range g.unknown {
		if kind != "" && u.Kind != kind {
			continue
		}
		result = append(result, u)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Object.ID != result[j].Object.ID {
			return result[i].Object.ID < result[j].Object.ID
		}
		return result[i].Kind < result[j].Kind
	})
	return result
}

// DeleteUnknowns returns the uncertainties that matter when deleting target:
// uncertainties on target itself (when target is an image/volume/network),
// plus every uncertainty of the matching reference kind across all containers
// — any of them could point at target, so they are reported rather than
// assumed not to. Container/service targets have no incoming references, so
// their own (outgoing) uncertainties do not block their removal.
func (g *RelationGraph) DeleteUnknowns(target ObjectRef) []Uncertainty {
	if g == nil {
		return nil
	}
	kind := edgeKindForTarget(target.Type)
	seen := make(map[uncertaintyKey]bool)
	var result []Uncertainty
	for key, u := range g.unknown {
		if (kind != "" && key.node == nodeKeyOf(target)) || (kind != "" && u.Kind == kind) {
			if seen[key] {
				continue
			}
			seen[key] = true
			result = append(result, u)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Object.ID != result[j].Object.ID {
			return result[i].Object.ID < result[j].Object.ID
		}
		return result[i].Kind < result[j].Kind
	})
	return result
}

func edgeKindForTarget(t ObjectType) ReferenceKind {
	switch t {
	case ImageObject:
		return ImageReference
	case VolumeObject:
		return VolumeReference
	case NetworkObject:
		return NetworkReference
	default:
		return ""
	}
}

// graphBuilder accumulates a new graph. It is not published until fully built,
// so concurrent creates/deletes can only flip an object to "unknown", never
// leave the graph half updated.
type graphBuilder struct {
	graph *RelationGraph

	// Per object, observations from the list and inspect sources. If the two
	// sources disagree the object cannot be trusted and its edges of that
	// kind are withheld in favour of an uncertainty, so the graph can never
	// contain self-contradictory records.
	imageIDs    map[nodeKey][]string
	imageNames  map[nodeKey][]string
	volumeSets  map[nodeKey][][]string
	networkSets map[nodeKey][][]string
}

func newGraphBuilder() *graphBuilder {
	return &graphBuilder{
		graph: &RelationGraph{
			BuiltAt:           time.Now(),
			nodes:             make(map[nodeKey]bool),
			outgoing:          make(map[nodeKey][]Reference),
			incoming:          make(map[nodeKey][]Reference),
			serviceContainers: make(map[string][]ObjectRef),
			projectContainers: make(map[string][]ObjectRef),
			unknown:           make(map[uncertaintyKey]Uncertainty),
		},
		imageIDs:    make(map[nodeKey][]string),
		imageNames:  make(map[nodeKey][]string),
		volumeSets:  make(map[nodeKey][][]string),
		networkSets: make(map[nodeKey][][]string),
	}
}

// BuildRelationGraph collects the references between containers and images,
// volumes, networks, services and projects. Every observed object becomes a
// node; edges are attributed to the container's project and service.
func BuildRelationGraph(
	containers []*Container,
	images []*Image,
	volumes []*Volume,
	networks []*Network,
	services []*Service,
) *RelationGraph {
	b := newGraphBuilder()
	g := b.graph

	imageByID := make(map[string]*Image, len(images))
	for _, img := range images {
		ref := ObjectRef{Type: ImageObject, ID: img.ID, Name: imageDisplayName(img)}
		g.nodes[nodeKeyOf(ref)] = true
		imageByID[img.ID] = img
	}

	for _, vol := range volumes {
		g.nodes[nodeKeyOf(ObjectRef{Type: VolumeObject, ID: vol.Name, Name: vol.Name})] = true
	}

	for _, nw := range networks {
		g.nodes[nodeKeyOf(ObjectRef{Type: NetworkObject, ID: nw.Name, Name: nw.Name})] = true
	}

	projectNames := make(map[string]bool)
	for _, svc := range services {
		ref := ObjectRef{
			Type: ServiceObject,
			ID:   svc.ProjectName + "-" + svc.Name,
			Name: svc.Name,
		}
		g.nodes[nodeKeyOf(ref)] = true
		if svc.ProjectName != "" {
			projectNames[svc.ProjectName] = true
		}
	}

	for _, ctr := range containers {
		ref := ObjectRef{Type: ContainerObject, ID: ctr.ID, Name: ctr.Name}
		g.nodes[nodeKeyOf(ref)] = true

		if ctr.ProjectName != "" {
			projectNames[ctr.ProjectName] = true
			g.projectContainers[ctr.ProjectName] = append(g.projectContainers[ctr.ProjectName], ref)
			if ctr.ServiceName != "" && !ctr.OneOff {
				serviceID := ctr.ProjectName + "-" + ctr.ServiceName
				g.serviceContainers[serviceID] = append(g.serviceContainers[serviceID], ref)
			}
		}

		b.collectContainer(ctr, ref, imageByID)
	}

	for name := range projectNames {
		g.nodes[nodeKeyOf(ObjectRef{Type: ProjectObject, ID: name, Name: name})] = true
	}

	b.crossCheckImageCounts(images)

	return g
}

func imageDisplayName(img *Image) string {
	if len(img.Image.RepoTags) > 0 {
		return img.Image.RepoTags[0]
	}
	return img.ID
}

func (b *graphBuilder) collectContainer(ctr *Container, ref ObjectRef, imageByID map[string]*Image) {
	detailsLoaded := ctr.DetailsLoaded()

	// ---- image reference ----
	if ctr.Container.ImageID != "" {
		b.imageIDs[nodeKeyOf(ref)] = append(b.imageIDs[nodeKeyOf(ref)], normalizeID(ctr.Container.ImageID))
	}
	if name := strings.TrimSpace(ctr.Container.Image); ctr.Container.ImageID == "" && name != "" {
		// list only carried an image name
		b.imageNames[nodeKeyOf(ref)] = append(b.imageNames[nodeKeyOf(ref)], name)
	}
	if detailsLoaded {
		if id := normalizeID(ctr.Details.Image); id != "" {
			b.imageIDs[nodeKeyOf(ref)] = append(b.imageIDs[nodeKeyOf(ref)], id)
		}
		if ctr.Details.Config != nil && strings.TrimSpace(ctr.Details.Config.Image) != "" {
			b.imageNames[nodeKeyOf(ref)] = append(b.imageNames[nodeKeyOf(ref)], strings.TrimSpace(ctr.Details.Config.Image))
		}
	}
	b.resolveImage(ref, imageByID, detailsLoaded, ctr.ProjectName, ctr.effectiveServiceName())

	// ---- volume references ----
	listVolumes := volumeMountNames(ctr.Container.Mounts)
	b.volumeSets[nodeKeyOf(ref)] = append(b.volumeSets[nodeKeyOf(ref)], listVolumes)
	if detailsLoaded {
		b.volumeSets[nodeKeyOf(ref)] = append(b.volumeSets[nodeKeyOf(ref)], volumeMountNames(ctr.Details.Mounts))
	}
	volumes := b.resolveSet(ref, b.volumeSets[nodeKeyOf(ref)], VolumeReference, detailsLoaded)
	for _, name := range volumes {
		target := ObjectRef{Type: VolumeObject, ID: name, Name: name}
		detail := mountDestination(ctr, name, detailsLoaded)
		b.addEdge(Reference{
			Referrer: ref,
			Target:   target,
			Kind:     VolumeReference,
			Project:  ctr.ProjectName,
			Service:  ctr.effectiveServiceName(),
			Detail:   detail,
		})
	}

	// ---- network references ----
	listNetworks := listNetworkNames(ctr.Container.NetworkSettings)
	if ctr.Container.NetworkSettings != nil {
		b.networkSets[nodeKeyOf(ref)] = append(b.networkSets[nodeKeyOf(ref)], listNetworks)
	}
	if detailsLoaded && ctr.Details.NetworkSettings != nil {
		b.networkSets[nodeKeyOf(ref)] = append(b.networkSets[nodeKeyOf(ref)], mapKeys(ctr.Details.NetworkSettings.Networks))
	}
	networkNames := b.resolveNetworks(ref, ctr, detailsLoaded)
	for _, name := range networkNames {
		target := ObjectRef{Type: NetworkObject, ID: name, Name: name}
		b.addEdge(Reference{
			Referrer: ref,
			Target:   target,
			Kind:     NetworkReference,
			Project:  ctr.ProjectName,
			Service:  ctr.effectiveServiceName(),
			Detail:   networkDetail(ctr, name, detailsLoaded),
		})
	}
}

// effectiveServiceName is the service the edge should be attributed to.
// One-off containers are not attributed to a service even if they carry a
// service label.
func (c *Container) effectiveServiceName() string {
	if c.OneOff {
		return ""
	}
	return c.ServiceName
}

func normalizeID(id string) string {
	id = strings.TrimSpace(id)
	// docker inspect sometimes prefixes with "sha256:" while the image list
	// id already includes it; normalise so the two sources compare cleanly.
	if len(id) >= 7 && id[:7] == "sha256:" {
		return id
	}
	return "sha256:" + strings.TrimPrefix(id, "sha256:")
}

func volumeMountNames(mounts []container.MountPoint) []string {
	names := make([]string, 0, len(mounts))
	for _, m := range mounts {
		if m.Type == mount.TypeVolume && m.Name != "" {
			names = append(names, m.Name)
		}
	}
	sort.Strings(names)
	return names
}

func listNetworkNames(settings *container.NetworkSettingsSummary) []string {
	if settings == nil {
		return nil
	}
	return mapKeys(settings.Networks)
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func mountDestination(ctr *Container, volumeName string, detailsLoaded bool) string {
	if detailsLoaded {
		for _, m := range ctr.Details.Mounts {
			if m.Type == mount.TypeVolume && m.Name == volumeName {
				return m.Destination
			}
		}
	}
	for _, m := range ctr.Container.Mounts {
		if m.Type == mount.TypeVolume && m.Name == volumeName {
			return m.Destination
		}
	}
	return ""
}

func networkDetail(ctr *Container, networkName string, detailsLoaded bool) string {
	if detailsLoaded && ctr.Details.NetworkSettings != nil {
		if endpoint, ok := ctr.Details.NetworkSettings.Networks[networkName]; ok && endpoint != nil {
			return endpoint.IPAddress
		}
	}
	return ""
}

// resolveImage reconciles the image observations. Disagreement between the
// list and inspect observations flips the relation to unknown instead of
// publishing contradictory edges.
func (b *graphBuilder) resolveImage(ref ObjectRef, imageByID map[string]*Image, detailsLoaded bool, project, service string) {
	ids := uniqueSorted(b.imageIDs[nodeKeyOf(ref)])
	names := uniqueSorted(b.imageNames[nodeKeyOf(ref)])

	switch {
	case len(ids) == 1:
		target := ObjectRef{Type: ImageObject, ID: ids[0]}
		if img, ok := imageByID[ids[0]]; ok {
			target.Name = imageDisplayName(img)
		} else if len(names) > 0 {
			target.Name = names[0]
		} else {
			target.Name = ids[0]
		}
		b.addEdge(Reference{
			Referrer: ref,
			Target:   target,
			Kind:     ImageReference,
			Project:  project,
			Service:  service,
			Detail:   strings.Join(names, ", "),
		})
	case len(ids) > 1:
		b.markUnknown(ref, ImageReference, fmt.Sprintf(
			"image id is contradictory between sources: %s; possible concurrent change", strings.Join(ids, ", ")))
	case len(names) == 1:
		// only an image name is known (no inspect, no image id)
		b.addEdge(Reference{
			Referrer: ref,
			Target:   ObjectRef{Type: ImageObject, ID: names[0], Name: names[0]},
			Kind:     ImageReference,
			Project:  project,
			Service:  service,
			Detail:   names[0],
		})
	case len(names) > 1:
		b.markUnknown(ref, ImageReference, fmt.Sprintf(
			"image reference is contradictory between sources: %s", strings.Join(names, ", ")))
	default:
		// neither source produced anything usable
		if !detailsLoaded {
			b.markUnknown(ref, ImageReference, "image reference could not be read: container details unavailable and list carried no image id")
		}
	}
}

// resolveSet reconciles two observations of a name set. Empty observations are
// kept distinct from "no observation": with a single observation the set is
// trusted; with two agreeing observations the set is definite; with two
// disagreeing observations the relation flips to unknown.
func (b *graphBuilder) resolveSet(ref ObjectRef, observations [][]string, kind ReferenceKind, detailsLoaded bool) []string {
	if len(observations) == 0 {
		if !detailsLoaded {
			b.markUnknown(ref, kind, fmt.Sprintf("%s references could not be read: neither list nor details were available", kind))
		}
		return nil
	}
	if len(observations) == 1 {
		return observations[0]
	}
	if equalStringSets(observations[0], observations[1]) {
		return observations[0]
	}
	b.markUnknown(ref, kind, fmt.Sprintf(
		"%s references disagree between list (%s) and inspect (%s); possible concurrent change",
		kind, joinOrNone(observations[0]), joinOrNone(observations[1])))
	return nil
}

// resolveNetworks is like resolveSet but distinguishes "no observation" (both
// list and inspect missing the network data) from an observation of an empty
// set, which means the container is connected to no networks.
func (b *graphBuilder) resolveNetworks(ref ObjectRef, ctr *Container, detailsLoaded bool) []string {
	observations := b.networkSets[nodeKeyOf(ref)]

	listHasNetData := ctr.Container.NetworkSettings != nil
	inspectHasNetData := detailsLoaded && ctr.Details.NetworkSettings != nil

	if !listHasNetData && !inspectHasNetData {
		b.markUnknown(ref, NetworkReference, "network membership could not be read: neither list nor details carried network settings")
		return nil
	}
	return b.resolveSet(ref, observations, NetworkReference, detailsLoaded)
}

func (b *graphBuilder) addEdge(edge Reference) {
	g := b.graph
	if !edge.Referrer.Type.Valid() || !edge.Target.Type.Valid() {
		return
	}
	g.outgoing[nodeKeyOf(edge.Referrer)] = append(g.outgoing[nodeKeyOf(edge.Referrer)], edge)
	g.incoming[nodeKeyOf(edge.Target)] = append(g.incoming[nodeKeyOf(edge.Target)], edge)
}

func (b *graphBuilder) markUnknown(object ObjectRef, kind ReferenceKind, reason string) {
	key := uncertaintyKey{node: nodeKeyOf(object), kind: kind}
	b.graph.unknown[key] = Uncertainty{Object: object, Kind: kind, Reason: reason}
}

// crossCheckImageCounts compares the daemon-provided container count on each
// image summary (when present) against the count derived from the graph. A
// mismatch means the state was changing during collection and is reported.
func (b *graphBuilder) crossCheckImageCounts(images []*Image) {
	for _, img := range images {
		if img.Image.Containers < 0 {
			continue
		}
		ref := ObjectRef{Type: ImageObject, ID: img.ID, Name: imageDisplayName(img)}
		derived := int64(len(b.graph.incoming[nodeKeyOf(ref)]))
		if derived != img.Image.Containers {
			b.markUnknown(ref, ImageReference, fmt.Sprintf(
				"image container count is inconsistent: daemon reports %d but the collected relations show %d; possible concurrent change",
				img.Image.Containers, derived))
		}
	}
}

// Valid reports whether the object type is one the graph understands.
func (t ObjectType) Valid() bool {
	switch t {
	case ContainerObject, ImageObject, VolumeObject, NetworkObject, ServiceObject, ProjectObject:
		return true
	default:
		return false
	}
}

func uniqueSorted(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func joinOrNone(set []string) string {
	if len(set) == 0 {
		return "none"
	}
	return strings.Join(set, ", ")
}
