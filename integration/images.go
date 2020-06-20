/*
Copyright 2018 Google LLC

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

package integration

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleContainerTools/kaniko/pkg/timing"
)

const (
	// ExecutorImage is the name of the kaniko executor image
	ExecutorImage = "executor-image"
	//WarmerImage is the name of the kaniko cache warmer image
	WarmerImage = "warmer-image"

	dockerPrefix     = "docker-"
	kanikoPrefix     = "kaniko-"
	buildContextPath = "/workspace"
	cacheDir         = "/workspace/cache"
	baseImageToCache = "gcr.io/google-appengine/debian9@sha256:1d6a9a6d106bd795098f60f4abb7083626354fa6735e81743c7f8cfca11259f0"
)

// Arguments to build Dockerfiles with, used for both docker and kaniko builds
var argsMap = map[string][]string{
	"Dockerfile_test_run":        {"file=/file"},
	"Dockerfile_test_run_new":    {"file=/file"},
	"Dockerfile_test_run_redo":   {"file=/file"},
	"Dockerfile_test_workdir":    {"workdir=/arg/workdir"},
	"Dockerfile_test_add":        {"file=context/foo"},
	"Dockerfile_test_arg_secret": {"SSH_PRIVATE_KEY", "SSH_PUBLIC_KEY=Pµbl1cK€Y"},
	"Dockerfile_test_onbuild":    {"file=/tmp/onbuild"},
	"Dockerfile_test_scratch": {
		"image=scratch",
		"hello=hello-value",
		"file=context/foo",
		"file3=context/b*",
	},
	"Dockerfile_test_multistage": {"file=/foo2"},
}

// Environment to build Dockerfiles with, used for both docker and kaniko builds
var envsMap = map[string][]string{
	"Dockerfile_test_arg_secret": {"SSH_PRIVATE_KEY=ThEPriv4t3Key"},
}

// Arguments to build Dockerfiles with when building with docker
var additionalDockerFlagsMap = map[string][]string{
	"Dockerfile_test_target": {"--target=second"},
}

// Arguments to build Dockerfiles with when building with kaniko
var additionalKanikoFlagsMap = map[string][]string{
	"Dockerfile_test_add":        {"--single-snapshot"},
	"Dockerfile_test_run_new":    {"--use-new-run=true"},
	"Dockerfile_test_run_redo":   {"--snapshotMode=redo"},
	"Dockerfile_test_scratch":    {"--single-snapshot"},
	"Dockerfile_test_maintainer": {"--single-snapshot"},
	"Dockerfile_test_target":     {"--target=second"},
}

// output check to do when building with kaniko
var outputChecks = map[string]func(string, []byte) error{
	"Dockerfile_test_arg_secret": checkArgsNotPrinted,
}

// Checks if argument are not printed in output.
// Argument may be passed through --build-arg key=value manner or --build-arg key with key in environment
func checkArgsNotPrinted(dockerfile string, out []byte) error {
	for _, arg := range argsMap[dockerfile] {
		argSplitted := strings.Split(arg, "=")
		if len(argSplitted) == 2 {
			if idx := bytes.Index(out, []byte(argSplitted[1])); idx >= 0 {
				return fmt.Errorf("Argument value %s for argument %s displayed in output", argSplitted[1], argSplitted[0])
			}
		} else if len(argSplitted) == 1 {
			if envs, ok := envsMap[dockerfile]; ok {
				for _, env := range envs {
					envSplitted := strings.Split(env, "=")
					if len(envSplitted) == 2 {
						if idx := bytes.Index(out, []byte(envSplitted[1])); idx >= 0 {
							return fmt.Errorf("Argument value %s for argument %s displayed in output", envSplitted[1], argSplitted[0])
						}
					}
				}
			}
		}
	}
	return nil
}

var bucketContextTests = []string{"Dockerfile_test_copy_bucket"}
var reproducibleTests = []string{"Dockerfile_test_reproducible"}

// GetDockerImage constructs the name of the docker image that would be built with
// dockerfile if it was tagged with imageRepo.
func GetDockerImage(imageRepo, dockerfile string) string {
	return strings.ToLower(imageRepo + dockerPrefix + dockerfile)
}

// GetKanikoImage constructs the name of the kaniko image that would be built with
// dockerfile if it was tagged with imageRepo.
func GetKanikoImage(imageRepo, dockerfile string) string {
	return strings.ToLower(imageRepo + kanikoPrefix + dockerfile)
}

// GetVersionedKanikoImage versions constructs the name of the kaniko image that would be built
// with the dockerfile and versions it for cache testing
func GetVersionedKanikoImage(imageRepo, dockerfile string, version int) string {
	return strings.ToLower(imageRepo + kanikoPrefix + dockerfile + strconv.Itoa(version))
}

// FindDockerFiles will look for test docker files in the directory dockerfilesPath.
// These files must start with `Dockerfile_test`. If the file is one we are intentionally
// skipping, it will not be included in the returned list.
func FindDockerFiles(dockerfilesPath string) ([]string, error) {
	allDockerfiles, err := filepath.Glob(path.Join(dockerfilesPath, "Dockerfile_test*"))
	if err != nil {
		return []string{}, fmt.Errorf("Failed to find docker files at %s: %s", dockerfilesPath, err)
	}

	var dockerfiles []string
	for _, dockerfile := range allDockerfiles {
		// Remove the leading directory from the path
		dockerfile = dockerfile[len("dockerfiles/"):]
		dockerfiles = append(dockerfiles, dockerfile)

	}
	return dockerfiles, err
}

// DockerFileBuilder knows how to build docker files using both Kaniko and Docker and
// keeps track of which files have been built.
type DockerFileBuilder struct {
	// Holds all available docker files and whether or not they've been built
	filesBuilt           map[string]struct{}
	DockerfilesToIgnore  map[string]struct{}
	TestCacheDockerfiles map[string]struct{}
}

// NewDockerFileBuilder will create a DockerFileBuilder initialized with dockerfiles, which
// it will assume are all as yet unbuilt.
func NewDockerFileBuilder() *DockerFileBuilder {
	d := DockerFileBuilder{filesBuilt: map[string]struct{}{}}
	d.DockerfilesToIgnore = map[string]struct{}{
		"Dockerfile_test_add_404": {},
		// TODO: remove test_user_run from this when https://github.com/GoogleContainerTools/container-diff/issues/237 is fixed
		"Dockerfile_test_user_run": {},
	}
	d.TestCacheDockerfiles = map[string]struct{}{
		"Dockerfile_test_cache":         {},
		"Dockerfile_test_cache_install": {},
		"Dockerfile_test_cache_perm":    {},
		"Dockerfile_test_cache_copy":    {},
	}
	return &d
}

func addServiceAccountFlags(flags []string, serviceAccount string) []string {
	if len(serviceAccount) > 0 {
		flags = append(flags, "-e",
			"GOOGLE_APPLICATION_CREDENTIALS=/secret/"+filepath.Base(serviceAccount),
			"-v", filepath.Dir(serviceAccount)+":/secret/")
	} else {
		flags = append(flags, "-v", os.Getenv("HOME")+"/.config/gcloud:/root/.config/gcloud")
	}
	return flags
}

func (d *DockerFileBuilder) BuildDockerImage(imageRepo, dockerfilesPath, dockerfile, contextDir string) error {
	fmt.Printf("Building image for Dockerfile %s\n", dockerfile)

	var buildArgs []string
	buildArgFlag := "--build-arg"
	for _, arg := range argsMap[dockerfile] {
		buildArgs = append(buildArgs, buildArgFlag, arg)
	}

	// build docker image
	additionalFlags := append(buildArgs, additionalDockerFlagsMap[dockerfile]...)
	dockerImage := strings.ToLower(imageRepo + dockerPrefix + dockerfile)

	dockerArgs := []string{
		"build",
		"-t", dockerImage,
	}

	if dockerfilesPath != "" {
		dockerArgs = append(dockerArgs, "-f", path.Join(dockerfilesPath, dockerfile))
	}

	dockerArgs = append(dockerArgs, contextDir)
	dockerArgs = append(dockerArgs, additionalFlags...)

	dockerCmd := exec.Command("docker", dockerArgs...)
	if env, ok := envsMap[dockerfile]; ok {
		dockerCmd.Env = append(dockerCmd.Env, env...)
	}

	out, err := RunCommandWithoutTest(dockerCmd)
	if err != nil {
		return fmt.Errorf("Failed to build image %s with docker command \"%s\": %s %s", dockerImage, dockerCmd.Args, err, string(out))
	}
	fmt.Printf("Build image for Dockerfile %s as %s. docker build output: %s \n", dockerfile, dockerImage, out)
	return nil
}

// BuildImage will build dockerfile (located at dockerfilesPath) using both kaniko and docker.
// The resulting image will be tagged with imageRepo. If the dockerfile will be built with
// context (i.e. it is in `buildContextTests`) the context will be pulled from gcsBucket.
func (d *DockerFileBuilder) BuildImage(config *integrationTestConfig, dockerfilesPath, dockerfile string) error {
	_, ex, _, _ := runtime.Caller(0)
	cwd := filepath.Dir(ex)

	return d.BuildImageWithContext(config, dockerfilesPath, dockerfile, cwd)
}

func (d *DockerFileBuilder) BuildImageWithContext(config *integrationTestConfig, dockerfilesPath, dockerfile, contextDir string) error {
	if _, present := d.filesBuilt[dockerfile]; present {
		return nil
	}
	gcsBucket, serviceAccount, imageRepo := config.gcsBucket, config.serviceAccount, config.imageRepo

	var buildArgs []string
	buildArgFlag := "--build-arg"
	for _, arg := range argsMap[dockerfile] {
		buildArgs = append(buildArgs, buildArgFlag, arg)
	}

	timer := timing.Start(dockerfile + "_docker")
	d.BuildDockerImage(imageRepo, dockerfilesPath, dockerfile, contextDir)
	timing.DefaultRun.Stop(timer)

	contextFlag := "-c"
	contextPath := buildContextPath
	for _, d := range bucketContextTests {
		if d == dockerfile {
			contextFlag = "-b"
			contextPath = gcsBucket
		}
	}

	additionalKanikoFlags := additionalKanikoFlagsMap[dockerfile]
	additionalKanikoFlags = append(additionalKanikoFlags, contextFlag, contextPath)
	for _, d := range reproducibleTests {
		if d == dockerfile {
			additionalKanikoFlags = append(additionalKanikoFlags, "--reproducible")
			break
		}
	}

	kanikoImage := GetKanikoImage(imageRepo, dockerfile)
	timer = timing.Start(dockerfile + "_kaniko")
	if _, err := buildKanikoImage(dockerfilesPath, dockerfile, buildArgs, additionalKanikoFlags, kanikoImage,
		contextDir, gcsBucket, serviceAccount, true); err != nil {
		return err
	}
	timing.DefaultRun.Stop(timer)

	d.filesBuilt[dockerfile] = struct{}{}

	return nil
}

func populateVolumeCache() error {
	_, ex, _, _ := runtime.Caller(0)
	cwd := filepath.Dir(ex)
	warmerCmd := exec.Command("docker",
		append([]string{"run", "--net=host",
			"-d",
			"-v", os.Getenv("HOME") + "/.config/gcloud:/root/.config/gcloud",
			"-v", cwd + ":/workspace",
			WarmerImage,
			"-c", cacheDir,
			"-i", baseImageToCache},
		)...,
	)

	if _, err := RunCommandWithoutTest(warmerCmd); err != nil {
		return fmt.Errorf("Failed to warm kaniko cache: %s", err)
	}

	return nil
}

// buildCachedImages builds the images for testing caching via kaniko where version is the nth time this image has been built
func (d *DockerFileBuilder) buildCachedImages(config *integrationTestConfig, cacheRepo, dockerfilesPath string, version int) error {
	imageRepo, serviceAccount := config.imageRepo, config.serviceAccount
	_, ex, _, _ := runtime.Caller(0)
	cwd := filepath.Dir(ex)

	cacheFlag := "--cache=true"

	for dockerfile := range d.TestCacheDockerfiles {
		benchmarkEnv := "BENCHMARK_FILE=false"
		if b, err := strconv.ParseBool(os.Getenv("BENCHMARK")); err == nil && b {
			os.Mkdir("benchmarks", 0755)
			benchmarkEnv = "BENCHMARK_FILE=/workspace/benchmarks/" + dockerfile
		}
		kanikoImage := GetVersionedKanikoImage(imageRepo, dockerfile, version)

		dockerRunFlags := []string{"run", "--net=host",
			"-v", cwd + ":/workspace",
			"-e", benchmarkEnv}
		dockerRunFlags = addServiceAccountFlags(dockerRunFlags, serviceAccount)
		dockerRunFlags = append(dockerRunFlags, ExecutorImage,
			"-f", path.Join(buildContextPath, dockerfilesPath, dockerfile),
			"-d", kanikoImage,
			"-c", buildContextPath,
			cacheFlag,
			"--cache-repo", cacheRepo,
			"--cache-dir", cacheDir)
		kanikoCmd := exec.Command("docker", dockerRunFlags...)

		_, err := RunCommandWithoutTest(kanikoCmd)
		if err != nil {
			return fmt.Errorf("Failed to build cached image %s with kaniko command \"%s\": %s", kanikoImage, kanikoCmd.Args, err)
		}
	}
	return nil
}

// buildRelativePathsImage builds the images for testing passing relatives paths to Kaniko
func (d *DockerFileBuilder) buildRelativePathsImage(imageRepo, dockerfile, serviceAccount, buildContextPath string) error {
	_, ex, _, _ := runtime.Caller(0)
	cwd := filepath.Dir(ex)

	dockerImage := GetDockerImage(imageRepo, "test_relative_"+dockerfile)
	kanikoImage := GetKanikoImage(imageRepo, "test_relative_"+dockerfile)

	dockerCmd := exec.Command("docker",
		append([]string{"build",
			"-t", dockerImage,
			"-f", dockerfile,
			"./context"},
		)...,
	)

	timer := timing.Start(dockerfile + "_docker")
	out, err := RunCommandWithoutTest(dockerCmd)
	timing.DefaultRun.Stop(timer)
	if err != nil {
		return fmt.Errorf("Failed to build image %s with docker command \"%s\": %s %s", dockerImage, dockerCmd.Args, err, string(out))
	}

	dockerRunFlags := []string{"run", "--net=host", "-v", cwd + ":/workspace"}
	dockerRunFlags = addServiceAccountFlags(dockerRunFlags, serviceAccount)
	dockerRunFlags = append(dockerRunFlags, ExecutorImage,
		"-f", dockerfile,
		"-d", kanikoImage,
		"-c", buildContextPath)

	kanikoCmd := exec.Command("docker", dockerRunFlags...)

	timer = timing.Start(dockerfile + "_kaniko_relative_paths")
	out, err = RunCommandWithoutTest(kanikoCmd)
	timing.DefaultRun.Stop(timer)

	if err != nil {
		return fmt.Errorf(
			"Failed to build relative path image %s with kaniko command \"%s\": %s\n%s",
			kanikoImage, kanikoCmd.Args, err, string(out))
	}

	return nil
}

func buildKanikoImage(
	dockerfilesPath string,
	dockerfile string,
	buildArgs []string,
	kanikoArgs []string,
	kanikoImage string,
	contextDir string,
	gcsBucket string,
	serviceAccount string,
	shdUpload bool,
) (string, error) {
	benchmarkEnv := "BENCHMARK_FILE=false"
	benchmarkDir, err := ioutil.TempDir("", "")
	if err != nil {
		return "", err
	}
	if b, err := strconv.ParseBool(os.Getenv("BENCHMARK")); err == nil && b {
		benchmarkEnv = "BENCHMARK_FILE=/kaniko/benchmarks/" + dockerfile
		if shdUpload {
			benchmarkFile := path.Join(benchmarkDir, dockerfile)
			fileName := fmt.Sprintf("run_%s_%s", time.Now().Format("2006-01-02-15:04"), dockerfile)
			dst := path.Join("benchmarks", fileName)
			defer UploadFileToBucket(gcsBucket, benchmarkFile, dst)
		}
	}

	// build kaniko image
	additionalFlags := append(buildArgs, kanikoArgs...)
	fmt.Printf("Going to build image with kaniko: %s, flags: %s \n", kanikoImage, additionalFlags)

	dockerRunFlags := []string{"run", "--net=host",
		"-e", benchmarkEnv,
		"-v", contextDir + ":/workspace",
		"-v", benchmarkDir + ":/kaniko/benchmarks",
	}

	if env, ok := envsMap[dockerfile]; ok {
		for _, envVariable := range env {
			dockerRunFlags = append(dockerRunFlags, "-e", envVariable)
		}
	}

	dockerRunFlags = addServiceAccountFlags(dockerRunFlags, serviceAccount)

	kanikoDockerfilePath := path.Join(buildContextPath, dockerfilesPath, dockerfile)
	if dockerfilesPath == "" {
		kanikoDockerfilePath = path.Join(buildContextPath, "Dockerfile")
	}

	dockerRunFlags = append(dockerRunFlags, ExecutorImage,
		"-f", kanikoDockerfilePath,
		"-d", kanikoImage)
	dockerRunFlags = append(dockerRunFlags, additionalFlags...)

	kanikoCmd := exec.Command("docker", dockerRunFlags...)

	out, err := RunCommandWithoutTest(kanikoCmd)
	if err != nil {
		return "", fmt.Errorf("Failed to build image %s with kaniko command \"%s\": %s %s", kanikoImage, kanikoCmd.Args, err, string(out))
	}
	if outputCheck := outputChecks[dockerfile]; outputCheck != nil {
		if err := outputCheck(dockerfile, out); err != nil {
			return "", fmt.Errorf("Output check failed for image %s with kaniko command : %s %s", kanikoImage, err, string(out))
		}
	}
	return benchmarkDir, nil
}
