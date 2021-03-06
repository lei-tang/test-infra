// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strings"

	u "istio.io/test-infra/toolbox/util"
)

var (
	repo            = flag.String("repo", "", "Optional. Update dependencies of only this repository")
	owner           = flag.String("owner", "istio", "Github Owner or org")
	tokenFile       = flag.String("token_file", "", "File containing Github API Access Token")
	baseBranch      = flag.String("base_branch", "master", "Branch from which the deps update commit is based")
	hub             = flag.String("hub", "", "Where the testing images are hosted")
	updateExtDep    = flag.Bool("update_ext_dep", false, "Updates external dependencies")
	githubClnt      *u.GithubClient
	githubEnvoyClnt *u.GithubClient
)

const (
	istioDepsFile         = "istio.deps"
	prTitlePrefix         = "[BOT PR] to update dependencies of "
	prBody                = "This PR will be merged automatically once checks are successful."
	dependencyUpdateLabel = "dependency-update"

	// CI Artifacts URLs
	istioArtifcatsURL = "https://storage.googleapis.com/istio-artifacts/%s/%s/artifacts"
	istioctlSuffix    = "istioctl"
	debianSuffix      = "debs"

	// envoy
	envoyOwner    = "envoyproxy"
	envoyRepo     = "envoy"
	envoyRepoPath = envoyOwner + "/" + envoyRepo

	// Istio Repos
	istioRepo = "istio"
	pilotRepo = "pilot"
	authRepo  = "auth"
	mixerRepo = "mixer"
	proxyRepo = "proxy"
)

// Updates dependency objects in :deps to the latest stable version.
// ASSUMES that the branches are the same for the dependencies
// Generates an MD5 digest of the latest dependencies, useful in avoiding making duplicate
// branches of the same code change.
// Returns a list of dependencies that were stale and have just been updated
func updateDepSHAGetFingerPrint(repo string, deps *[]u.Dependency) (string, []u.Dependency, error) {
	var depChangeList []u.Dependency
	digest, err := githubClnt.GetHeadCommitSHA(repo, *baseBranch)
	if err != nil {
		return "", depChangeList, err
	}
	digest += *baseBranch + *hub
	for i, dep := range *deps {
		var commitSHA string
		if dep.RepoName == envoyRepoPath {
			if *updateExtDep {
				// update envoy sha only when specified
				commitSHA, err = githubEnvoyClnt.GetHeadCommitSHA(envoyRepo, *baseBranch)
				if err != nil {
					// skip update we could not find envoy sha for this branch
					commitSHA = dep.LastStableSHA
					log.Printf("no envoy proxy sha for branch %s\n", *baseBranch)
				} else {
					log.Printf("new envoy proxy sha is %s\n", commitSHA)
				}
			} else {
				// otherwise skip update
				commitSHA = dep.LastStableSHA
				log.Printf("skipping update of envoy proxy sha is %s\n", commitSHA)
			}
		} else {
			commitSHA, err = githubClnt.GetHeadCommitSHA(dep.RepoName, *baseBranch)
			if err != nil {
				return "", depChangeList, err
			}
		}
		digest += commitSHA
		if dep.LastStableSHA != commitSHA {
			(*deps)[i].LastStableSHA = commitSHA
			depChangeList = append(depChangeList, (*deps)[i])
		}
	}

	return u.GetMD5Hash(digest), depChangeList, nil
}

func generateArtifactURL(repo, ref, suffix string) string {
	baseURL := fmt.Sprintf(istioArtifcatsURL, repo, ref)
	return fmt.Sprintf("%s/%s", baseURL, suffix)
}

func extraUpdateForProxy(file, key, value string) error {
	newkey := "ISTIO_API_SHA256"
	url := fmt.Sprintf("https://github.com/%s/api/archive/%s.tar.gz", *owner, value)

	if key == "ENVOY_SHA" {
		newkey = "ENVOY_SHA256"
		url = fmt.Sprintf("https://github.com/%s/envoy/archive/%s.tar.gz", envoyOwner, value)
	}

	tmpfile, fileErr := ioutil.TempFile("", "")
	if fileErr != nil {
		log.Fatalf("Error while creating tempfile: %v\n", fileErr)
	}

	defer func() {
		if err := os.Remove(tmpfile.Name()); err != nil {
			log.Fatalf("Error during clean up: %v\n", err)
		}
	}()

	cmd := fmt.Sprintf("wget %s -O %s ", url, tmpfile.Name())

	if _, err := u.Shell(cmd); err != nil {
		return err
	}

	cmd = fmt.Sprintf("sha256sum %s | awk '{print $1}'", tmpfile.Name())
	sha256Value, commandErr := u.Shell(cmd)
	if commandErr != nil {
		return commandErr
	}
	if err := u.UpdateKeyValueInFile(file, newkey, strings.TrimSuffix(sha256Value, "\n")); err != nil {
		return err
	}
	return nil
}

// Updates the list of dependencies in repo to the latest stable references
func updateDeps(repo string, deps *[]u.Dependency, depChangeList *[]u.Dependency) error {
	for _, dep := range *deps {
		if dep.File == "" {
			continue
		}
		if err := u.UpdateKeyValueInFile(dep.File, dep.Name, dep.LastStableSHA); err != nil {
			return err
		}
		if repo != proxyRepo {
			continue
		}
		if err := extraUpdateForProxy(dep.File, dep.Name, dep.LastStableSHA); err != nil {
			return err
		}
	}
	if repo != istioRepo || len(*hub) == 0 {
		return nil
	}

	args := ""
	for _, updatedDep := range *depChangeList {
		switch updatedDep.RepoName {
		case mixerRepo:
			args += fmt.Sprintf("-x %s,%s ", *hub, updatedDep.LastStableSHA)
		case pilotRepo:
			istioctlURL := generateArtifactURL(pilotRepo, updatedDep.LastStableSHA, istioctlSuffix)
			debianURL := generateArtifactURL(pilotRepo, updatedDep.LastStableSHA, debianSuffix)
			args += fmt.Sprintf("-p %s,%s -i %s -P %s ", *hub, updatedDep.LastStableSHA, istioctlURL, debianURL)
		case authRepo:
			debianURL := generateArtifactURL(authRepo, updatedDep.LastStableSHA, debianSuffix)
			args += fmt.Sprintf("-c %s,%s -A %s ", *hub, updatedDep.LastStableSHA, debianURL)
		case proxyRepo:
			debianURL := generateArtifactURL(proxyRepo, updatedDep.LastStableSHA, debianSuffix)
			args += fmt.Sprintf("-r %s -E %s ", updatedDep.LastStableSHA, debianURL)
		default:
			return fmt.Errorf("unknown dependency: %s", updatedDep.Name)
		}
	}
	cmd := fmt.Sprintf("./install/updateVersion.sh %s", args)
	_, err := u.Shell(cmd)
	return err
}

// Updates the given repository so that it uses the latest dependency references
// pushes new branch to remote, create pull request on given base branch,
// which is auto-merged after presumbit
func updateDependenciesOf(repo string) error {
	log.Printf("Updating dependencies of %s\n", repo)
	saveDir, err := os.Getwd()
	if err != nil {
		return err
	}
	repoDir, err := u.CloneRepoCheckoutBranch(githubClnt, repo, *baseBranch, "", "go/src/istio.io")
	if err != nil {
		return err
	}
	defer func() {
		if err = os.Chdir(saveDir); err != nil {
			log.Fatalf("Error during chdir: %v\n", err)
		}
		if err = u.RemoveLocalRepo("go"); err != nil {
			log.Fatalf("Error during clean up: %v\n", err)
		}
	}()
	deps, err := u.DeserializeDeps(istioDepsFile)
	// Don't fail if istio.deps file does not exist in the repo.
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("%s repo does not have istio.deps file to update.", repo)
			return nil
		}
		return err
	}
	fingerPrint, depChangeList, err := updateDepSHAGetFingerPrint(repo, &deps)
	if err != nil {
		return err
	}
	branch := "autoUpdateDeps_" + fingerPrint

	// First try to cleanup old PRs
	if err = githubClnt.CloseIdlePullRequests(
		prTitlePrefix, repo, *baseBranch); err != nil {
		log.Printf("error while closing idle PRs: %v\n", err)
	}
	// If the same branch still exists (which means it's not old enough), leave it there and don't do anything in this cycle
	exists, err := githubClnt.ExistBranch(repo, branch)
	if err != nil {
		return err
	}
	if exists {
		log.Printf("Branch %s exists", branch)
		return nil
	}

	if _, err = u.Shell("git checkout -b " + branch); err != nil {
		return err
	}
	if err = updateDeps(repo, &deps, &depChangeList); err != nil {
		return err
	}
	if err = u.SerializeDeps(istioDepsFile, &deps); err != nil {
		return err
	}
	if repo == istioRepo {
		goPath := path.Join(repoDir, "../../..")
		env := "GOPATH=" + goPath
		updateCommand := "; go get -u github.com/golang/dep/cmd/dep; dep ensure -update istio.io/api"
		if _, err = u.Shell(env + updateCommand); err != nil {
			return err
		}
		if *updateExtDep {
			// while depend update can introduce new changes,
			// introduce them only when requested
			if _, err = u.Shell("make depend.update"); err != nil {
				return err
			}
		}
	}
	if _, err = u.Shell("git diff -w --quiet HEAD"); err == nil {
		// diff exited without error, nothing to commit or only whitespace changes
		log.Printf("%s is up to date. No commits are made.", repo)
		return nil
	}
	// git is dirty so commit
	if err = u.CreateCommitPushToRemote(branch, "Update_Dependencies"); err != nil {
		return err
	}
	prTitle := prTitlePrefix + repo
	pr, err := githubClnt.CreatePullRequest(prTitle, prBody, "", branch, *baseBranch, repo)
	if err != nil {
		return err
	}
	if err := githubClnt.AddAutoMergeLabelsToPR(repo, pr); err != nil {
		return err
	}
	return githubClnt.AddlabelsToPR(repo, pr, dependencyUpdateLabel)
}

func init() {
	flag.Parse()
	if *tokenFile == "" {
		log.Fatalf("token_file not provided\n")
		return
	}
	token, err := u.GetAPITokenFromFile(*tokenFile)
	if err != nil {
		log.Fatalf("Error accessing user supplied token_file: %v\n", err)
	}
	githubClnt = u.NewGithubClient(*owner, token)
	githubEnvoyClnt = u.NewGithubClient(envoyOwner, token)
}

func main() {
	if *repo != "" { // only update dependencies of this repo
		if err := updateDependenciesOf(*repo); err != nil {
			log.Fatalf("Failed to update dependency: %v\n", err)
		}
	} else { // update dependencies of all repos in the istio project
		repos, err := githubClnt.ListRepos()
		if err != nil {
			log.Fatalf("Error when fetching list of repos: %v\n", err)
			return
		}
		for _, r := range repos {
			if err := updateDependenciesOf(r); err != nil {
				log.Fatalf("Failed to update dependency: %v\n", err)
			}
		}
	}
}
