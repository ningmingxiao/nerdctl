/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/progress"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/containerd/nerdctl/v2/pkg/api/types"
	"github.com/containerd/nerdctl/v2/pkg/containerdutil"
	"github.com/containerd/nerdctl/v2/pkg/containerutil"
	"github.com/containerd/nerdctl/v2/pkg/formatter"
	"github.com/containerd/nerdctl/v2/pkg/imgutil"
	"github.com/containerd/nerdctl/v2/pkg/labels"
)

// List prints containers according to `options`.
func List(ctx context.Context, client *containerd.Client, options types.ContainerListOptions) ([]ListItem, error) {
	containers, all, err := filterContainers(ctx, client, options.Filters, options.LastN, options.All)
	if err != nil {
		return nil, err
	}
	return prepareContainers(ctx, client, containers, options, all)
}

// filterContainers returns containers matching the filters.
//
//   - Supported filters: https://github.com/containerd/nerdctl/blob/main/docs/command-reference.md#whale-blue_square-nerdctl-ps
//   - all means showing all containers (default shows just running).
//   - lastN means only showing n last created containers (includes all states). Non-positive values are ignored.
//     In other words, if lastN is positive, all will be set to true.
func filterContainers(ctx context.Context, client *containerd.Client, filters []string, lastN int, all bool) ([]containerd.Container, bool, error) {
	containers, err := client.Containers(ctx)
	if err != nil {
		return nil, false, err
	}
	filterCtx, err := foldContainerFilters(ctx, containers, filters)
	if err != nil {
		return nil, false, err
	}
	containers = filterCtx.MatchesFilters(ctx)
	sort.Slice(containers, func(i, j int) bool {
		infoI, _ := containers[i].Info(ctx, containerd.WithoutRefreshedMetadata)
		infoJ, _ := containers[j].Info(ctx, containerd.WithoutRefreshedMetadata)
		return infoI.CreatedAt.After(infoJ.CreatedAt)
	})

	if lastN > 0 {
		all = true
		if lastN < len(containers) {
			containers = containers[:lastN]
		}
	}
	if all || filterCtx.all {
		return containers, true, nil
	}
	return containers, false, nil
}

type ListItem struct {
	Command   string
	CreatedAt time.Time
	ID        string
	Image     string
	Platform  string // nerdctl extension
	Names     string
	Ports     string
	Status    string
	Runtime   string // nerdctl extension
	Size      string
	Labels    string
	LabelsMap map[string]string `json:"-"`

	// TODO: "LocalVolumes", "Mounts", "Networks", "RunningFor", "State"
}

func (x *ListItem) Label(s string) string {
	return x.LabelsMap[s]
}

func prepareContainers(ctx context.Context, client *containerd.Client, containers []containerd.Container, options types.ContainerListOptions, all bool) ([]ListItem, error) {
	listItems := make([]ListItem, len(containers))
	snapshottersCache := map[string]snapshots.Snapshotter{}
	var wg sync.WaitGroup
	errChan := make(chan error, len(containers))
	for i, c := range containers {
		wg.Add(1)
		go func(ctx context.Context, c containerd.Container, i int) {
			defer wg.Done()
			info, err := c.Info(ctx, containerd.WithoutRefreshedMetadata)
			if err != nil {
				if errdefs.IsNotFound(err) {
					log.G(ctx).Warn(err)
					return
				}
				errChan <- err
			}
			spec, err := c.Spec(ctx)
			if err != nil {
				if errdefs.IsNotFound(err) {
					log.G(ctx).Warn(err)
					return
				}
				errChan <- err
			}
			id := c.ID()
			if options.Truncate && len(id) > 12 {
				id = id[:12]
			}
			cStatus := formatter.ContainerStatus(ctx, c)
			// only show Up status container
			if !all {
				if !strings.HasPrefix(cStatus, "Up") {
					return
				}
			}
			li := ListItem{
				Command:   formatter.InspectContainerCommand(spec, options.Truncate, true),
				CreatedAt: info.CreatedAt,
				ID:        id,
				Image:     info.Image,
				Platform:  info.Labels[labels.Platform],
				Names:     containerutil.GetContainerName(info.Labels),
				Ports:     formatter.FormatPorts(info.Labels),
				Status:    cStatus,
				Runtime:   info.Runtime.Name,
				Labels:    formatter.FormatLabels(info.Labels),
				LabelsMap: info.Labels,
			}
			if options.Size {
				snapshotter, ok := snapshottersCache[info.Snapshotter]
				if !ok {
					snapshottersCache[info.Snapshotter] = containerdutil.SnapshotService(client, info.Snapshotter)
					snapshotter = snapshottersCache[info.Snapshotter]
				}
				containerSize, err := getContainerSize(ctx, snapshotter, info.SnapshotKey)
				if err != nil {
					errChan <- err
				}
				li.Size = containerSize
			}
			listItems[i] = li
		}(ctx, c, i)
	}
	wg.Wait()
	close(errChan)
	if len(errChan) > 0 {
		errs := make([]error, len(errChan))
		for err := range errChan {
			errs = append(errs, err)
		}
		if len(errs) > 0 {
			return nil, combineErrors(errs)
		}
	}
	var result []ListItem
	for _, val := range listItems {
		if val.ID != "" {
			result = append(result, val)
		}
	}
	return result, nil
}

func combineErrors(errs []error) error {
	var errMsgs []string
	for _, err := range errs {
		if err != nil {
			errMsgs = append(errMsgs, err.Error())
		}
	}
	if len(errMsgs) > 0 {
		return errors.New(strings.Join(errMsgs, "; "))
	}
	return nil
}

func getContainerNetworks(containerLables map[string]string) []string {
	var networks []string
	if names, ok := containerLables[labels.Networks]; ok {
		if err := json.Unmarshal([]byte(names), &networks); err != nil {
			log.L.Warn(err)
		}
	}
	return networks
}

func getContainerSize(ctx context.Context, snapshotter snapshots.Snapshotter, snapshotKey string) (string, error) {
	// get container snapshot size
	var containerSize int64
	var imageSize int64

	if snapshotKey != "" {
		rw, all, err := imgutil.ResourceUsage(ctx, snapshotter, snapshotKey)
		if err != nil {
			return "", err
		}
		containerSize = rw.Size
		imageSize = all.Size
	}

	return fmt.Sprintf("%s (virtual %s)", progress.Bytes(containerSize).String(), progress.Bytes(imageSize).String()), nil
}
