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

package composer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/containerd/log"

	"github.com/containerd/nerdctl/v2/pkg/composer/serviceparser"
	"github.com/containerd/nerdctl/v2/pkg/internal/filesystem"
	"github.com/containerd/nerdctl/v2/pkg/labels"
)

func (c *Composer) upServices(ctx context.Context, parsedServices []*serviceparser.Service, uo UpOptions) error {
	if len(parsedServices) == 0 {
		return errors.New("no service was provided")
	}

	// TODO: parallelize loop for ensuring images (make sure not to mess up tty)
	for _, ps := range parsedServices {
		if err := c.ensureServiceImage(ctx, ps, !uo.NoBuild, uo.ForceBuild, BuildOptions{}, uo.QuietPull, uo.Pull); err != nil {
			return err
		}
	}

	recreate := uo.recreateStrategy()

	var (
		containers   = make(map[string]serviceparser.Container) // key: container ID
		services     = []string{}
		containersMu sync.Mutex
	)
	for _, ps := range parsedServices {
		ps := ps
		var runEG errgroup.Group
		services = append(services, ps.Unparsed.Name)
		for _, container := range ps.Containers {
			container := container
			runEG.Go(func() error {
				id, err := c.upServiceContainer(ctx, ps, container, recreate)
				if err != nil {
					return err
				}
				containersMu.Lock()
				containers[id] = container
				containersMu.Unlock()
				return nil
			})
		}
		if err := runEG.Wait(); err != nil {
			return err
		}
	}

	if uo.Detach {
		return nil
	}

	// this is used to stop containers in case --abort-on-container-exit flag is set.
	// c.Logs returns an error, so we don't need Ctrl-c to reach the "Stopping containers (forcibly)"
	if uo.AbortOnContainerExit {
		defer c.stopContainersFromParsedServices(ctx, containers)
	}
	log.G(ctx).Info("Attaching to logs")
	lo := LogsOptions{
		AbortOnContainerExit: uo.AbortOnContainerExit,
		Follow:               true,
		NoColor:              uo.NoColor,
		NoLogPrefix:          uo.NoLogPrefix,
		LatestRun:            recreate == RecreateNever,
	}
	if err := c.Logs(ctx, lo, services); err != nil {
		return err
	}

	log.G(ctx).Infof("Stopping containers (forcibly)") // TODO: support gracefully stopping
	c.stopContainersFromParsedServices(ctx, containers)
	return nil
}

func (c *Composer) ensureServiceImage(ctx context.Context, ps *serviceparser.Service, allowBuild, forceBuild bool, bo BuildOptions, quiet bool, pullModeArg string) error {
	if ps.Build != nil && allowBuild {
		if ps.Build.Force || forceBuild {
			return c.buildServiceImage(ctx, ps.Image, ps.Build, ps.Unparsed.Platform, bo)
		}
		if ok, err := c.ImageExists(ctx, ps.Image); err != nil {
			return err
		} else if !ok {
			return c.buildServiceImage(ctx, ps.Image, ps.Build, ps.Unparsed.Platform, bo)
		}
		// even when c.ImageExists returns true, we need to call c.EnsureImage
		// because ps.PullMode can be "always". So no return here.
		log.G(ctx).Debugf("Image %s already exists, not building", ps.Image)
	}

	log.G(ctx).Infof("Ensuring image %s", ps.Image)
	if pullModeArg != "" {
		return c.EnsureImage(ctx, ps.Image, pullModeArg, ps.Unparsed.Platform, ps, quiet)
	}
	return c.EnsureImage(ctx, ps.Image, ps.PullMode, ps.Unparsed.Platform, ps, quiet)
}

// upServiceContainer must be called after ensureServiceImage
// upServiceContainer returns container ID
func (c *Composer) upServiceContainer(ctx context.Context, service *serviceparser.Service, container serviceparser.Container, recreate string) (string, error) {
	// check if container already exists
	existingCid, err := c.containerID(ctx, container.Name, service.Unparsed.Name)
	if err != nil {
		return "", fmt.Errorf("error while checking for containers with name %q: %w", container.Name, err)
	}

	// FIXME
	if service.Unparsed.StdinOpen != service.Unparsed.Tty {
		return "", fmt.Errorf("currently StdinOpen(-i) and Tty(-t) should be same")
	}

	var runFlagD bool
	if !service.Unparsed.StdinOpen && !service.Unparsed.Tty {
		container.RunArgs = append([]string{"-d"}, container.RunArgs...)
		runFlagD = true
	}

	// start the existing container and exit early
	if existingCid != "" && recreate == RecreateNever {
		cmd := c.createNerdctlCmd(ctx, append([]string{"start"}, existingCid)...)
		if err := c.executeUpCmd(ctx, cmd, container.Name, runFlagD, service.Unparsed.StdinOpen); err != nil {
			return "", fmt.Errorf("error while starting existing container %s: %w", container.Name, err)
		}
		return existingCid, nil
	}

	// delete container if it already exists
	if existingCid != "" {
		log.G(ctx).Debugf("Container %q already exists, deleting", container.Name)
		delCmd := c.createNerdctlCmd(ctx, "rm", "-f", container.Name)
		if err = delCmd.Run(); err != nil {
			return "", fmt.Errorf("could not delete container %q: %w", container.Name, err)
		}
		log.G(ctx).Infof("Re-creating container %s", container.Name)
	} else {
		log.G(ctx).Infof("Creating container %s", container.Name)
	}

	for _, f := range container.Mkdir {
		log.G(ctx).Debugf("Creating a directory %q", f)
		if err = os.MkdirAll(f, 0o755); err != nil {
			return "", fmt.Errorf("failed to create a directory %q: %w", f, err)
		}
	}

	tempDir, err := os.MkdirTemp(os.TempDir(), "compose-")
	if err != nil {
		return "", fmt.Errorf("error while creating/re-creating container %s: %w", container.Name, err)
	}
	defer os.RemoveAll(tempDir)
	cidFilename := filepath.Join(tempDir, "cid")

	if c.EnvFile != "" {
		container.RunArgs = append([]string{"--env-file=" + c.EnvFile}, container.RunArgs...)
	}

	//add metadata labels to container https://github.com/compose-spec/compose-spec/blob/master/spec.md#labels
	container.RunArgs = append([]string{
		"--cidfile=" + cidFilename,
		fmt.Sprintf("-l=%s=%s", labels.ComposeProject, c.project.Name),
		fmt.Sprintf("-l=%s=%s", labels.ComposeService, service.Unparsed.Name),
	}, container.RunArgs...)

	cmd := c.createNerdctlCmd(ctx, append([]string{"run"}, container.RunArgs...)...)
	if c.DebugPrintFull {
		log.G(ctx).Debugf("Running %v", cmd.Args)
	}

	if err := c.executeUpCmd(ctx, cmd, container.Name, runFlagD, service.Unparsed.StdinOpen); err != nil {
		return "", fmt.Errorf("error while creating container %s: %w", container.Name, err)
	}

	cid, err := filesystem.ReadFile(cidFilename)
	if err != nil {
		return "", fmt.Errorf("error while creating container %s: %w", container.Name, err)
	}
	return strings.TrimSpace(string(cid)), nil
}

func (c *Composer) executeUpCmd(ctx context.Context, cmd *exec.Cmd, containerName string, runFlagD, stdinOpen bool) error {
	log.G(ctx).Infof("Running %v", cmd.Args)
	if c.DebugPrintFull {
		log.G(ctx).Debugf("Running %v", cmd.Args)
	}

	if stdinOpen {
		cmd.Stdin = os.Stdin
	}
	if !runFlagD {
		cmd.Stdout = os.Stdout
	}
	// Always propagate stderr to print detailed error messages (https://github.com/containerd/nerdctl/issues/1942)
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error while creating container %s: %w", containerName, err)
	}

	return nil
}
