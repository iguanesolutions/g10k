package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xorpaul/uiprogress"
)

func resolveGitRepositories(uniqueGitModules map[string]GitModule) {
	defer timeTrack(time.Now(), funcName())
	if len(uniqueGitModules) <= 0 {
		Debugf("uniqueGitModules[] is empty, skipping...")
		return
	}
	bar := uiprogress.AddBar(len(uniqueGitModules)).AppendCompleted().PrependElapsed()
	bar.PrependFunc(func(b *uiprogress.Bar) string {
		return fmt.Sprintf("Resolving Git modules (%d/%d)", b.Current(), len(uniqueGitModules))
	})
	// Dummy channel to coordinate the number of concurrent goroutines.
	// This channel should be buffered otherwise we will be immediately blocked
	// when trying to fill it.

	Debugf("Resolving " + strconv.Itoa(len(uniqueGitModules)) + " Git modules with " + strconv.Itoa(config.Maxworker) + " workers")
	concurrentGoroutines := make(chan struct{}, config.Maxworker)
	// Fill the dummy channel with config.Maxworker empty struct.
	for i := 0; i < config.Maxworker; i++ {
		concurrentGoroutines <- struct{}{}
	}

	// The done channel indicates when a single goroutine has finished its job.
	done := make(chan bool)
	// The waitForAllJobs channel allows the main program
	// to wait until we have indeed done all the jobs.
	waitForAllJobs := make(chan bool)
	// Collect all the jobs, and since the job is finished, we can
	// release another spot for a goroutine.
	go func() {
		for _, gm := range uniqueGitModules {
			go func(gm GitModule) {
				<-done
				// Say that another goroutine can now start.
				concurrentGoroutines <- struct{}{}
			}(gm)
		}
		// We have collected all the jobs, the program can now terminate
		waitForAllJobs <- true
	}()
	wg := sync.WaitGroup{}
	wg.Add(len(uniqueGitModules))

	for url, gm := range uniqueGitModules {
		Debugf("git repo url " + url)
		privateKey := gm.privateKey
		go func(url string, privateKey string, gm GitModule, bar *uiprogress.Bar) {
			// Try to receive from the concurrentGoroutines channel. When we have something,
			// it means we can start a new goroutine because another one finished.
			// Otherwise, it will block the execution until an execution
			// spot is available.
			<-concurrentGoroutines
			defer bar.Incr()
			defer wg.Done()

			if len(gm.privateKey) > 0 {
				Debugf("git repo url " + url + " with ssh key " + privateKey)
			} else {
				Debugf("git repo url " + url + " without ssh key")
			}

			//log.Println(config)
			// create save directory name from Git repo name
			repoDir := strings.Replace(strings.Replace(url, "/", "_", -1), ":", "-", -1)
			workDir := config.ModulesCacheDir + repoDir

			success := doMirrorOrUpdate(url, workDir, privateKey, gm.ignoreUnreachable, 1)
			if !success && config.UseCacheFallback == false {
				Fatalf("Fatal: Could not reach git repository " + url)
			}
			//	doCloneOrPull(source, workDir, targetDir, sa.Remote, branch, sa.PrivateKey)
            done <- true
		}(url, privateKey, gm, bar)
	}

	// Wait for all jobs to finish
	<-waitForAllJobs
	wg.Wait()
}

func doMirrorOrUpdate(url string, workDir string, sshPrivateKey string, allowFail bool, retryCount int) bool {
	needSSHKey := true
	if strings.Contains(url, "github.com") || len(sshPrivateKey) == 0 {
		needSSHKey = false
	}

	er := ExecResult{}
	gitCmd := "git clone --mirror " + url + " " + workDir
	if isDir(workDir) {
		gitCmd = "git --git-dir " + workDir + " remote update --prune"
	}

	if needSSHKey {
		er = executeCommand("ssh-agent bash -c 'ssh-add "+sshPrivateKey+"; "+gitCmd+"'", config.Timeout, allowFail)
	} else {
		er = executeCommand(gitCmd, config.Timeout, allowFail)
	}

	if er.returnCode != 0 {
		if config.UseCacheFallback {
			Warnf("WARN: git repository " + url + " does not exist or is unreachable at this moment!")
			Warnf("WARN: Trying to use cache for " + url + " git repository")
			return false
		} else if config.RetryGitCommands && retryCount > 0 {
			Warnf("WARN: git command failed: " + gitCmd + " deleting local cached repository and retrying...")
			purgeDir(workDir, "doMirrorOrUpdate, because git command failed, retrying")
			return doMirrorOrUpdate(url, workDir, sshPrivateKey, false, retryCount-1)
		}
		Warnf("WARN: git repository " + url + " does not exist or is unreachable at this moment!")
		return false
	}
	return true
}

func syncToModuleDir(srcDir string, targetDir string, tree string, allowFail bool, ignoreUnreachable bool, correspondingPuppetEnvironment string, onlyDelta bool) bool {
	startedAt := time.Now()
	mutex.Lock()
	syncGitCount++
	mutex.Unlock()
	if !isDir(srcDir) {
		if config.UseCacheFallback {
			Fatalf("Could not find cached git module " + srcDir)
		}
	}
	logCmd := "git --git-dir " + srcDir + " rev-parse --verify '" + tree
	if config.GitObjectSyntaxNotSupported != true {
		logCmd = logCmd + "^{object}'"
	} else {
		logCmd = logCmd + "'"
	}

	er := executeCommand(logCmd, config.Timeout, allowFail)
	hashFile := filepath.Join(targetDir, ".latest_commit")
	deployFile := filepath.Join(targetDir, ".g10k-deploy.json")
	needToSync := true
	if er.returnCode != 0 {
		if allowFail && ignoreUnreachable {
			Debugf("Failed to populate module " + targetDir + " but ignore-unreachable is set. Continuing...")
			purgeDir(targetDir, "syncToModuleDir, because ignore-unreachable is set for this module")
		}
		return false
	}

	if len(er.output) > 0 {
		if strings.HasPrefix(srcDir, config.EnvCacheDir) && fileExists(deployFile) {
			dr := readDeployResultFile(deployFile)
			if dr.Signature == strings.TrimSuffix(er.output, "\n") {
				needToSync = false
			}
		} else {
			targetHash, _ := ioutil.ReadFile(hashFile)
			if string(targetHash) == er.output {
				needToSync = false
				//Debugf("Skipping, because no diff found between " + srcDir + "(" + er.output + ") and " + targetDir + "(" + string(targetHash) + ")")
			}
		}

	}
	if onlyDelta {
		listGitRepoFiles(srcDir, tree, targetDir, hashFile)
	}
	if needToSync && er.returnCode == 0 {
		Infof("Need to sync " + targetDir)
		mutex.Lock()
		needSyncDirs = append(needSyncDirs, targetDir)
		if _, ok := needSyncEnvs[correspondingPuppetEnvironment]; !ok {
			needSyncEnvs[correspondingPuppetEnvironment] = empty
		}
		needSyncGitCount++
		mutex.Unlock()

		if !dryRun {
			if !onlyDelta {
				createOrPurgeDir(targetDir, "syncToModuleDir()")
			} else {
				checkDirAndCreate(targetDir, "git dir")
			}
			gitArchiveArgs := []string{"--git-dir", srcDir, "archive", tree}
			cmd := exec.Command("git", gitArchiveArgs...)
			Debugf("Executing git --git-dir " + srcDir + " archive " + tree)
			cmdOut, err := cmd.StdoutPipe()
			if err != nil {
				if !allowFail {
					Infof("Failed to populate module " + targetDir + " but ignore-unreachable is set. Continuing...")
				} else {
					return false
				}
				Fatalf("syncToModuleDir(): Failed to execute command: git --git-dir " + srcDir + " archive " + tree + " Error: " + err.Error())
			}
			cmd.Start()

			before := time.Now()
			unTar(cmdOut, targetDir)
			duration := time.Since(before).Seconds()
			mutex.Lock()
			ioGitTime += duration
			mutex.Unlock()

			err = cmd.Wait()
			if err != nil {
				Fatalf("syncToModuleDir(): Failed to execute command: git --git-dir " + srcDir + " archive " + tree + " Error: " + err.Error())
			}

			Verbosef("syncToModuleDir(): Executing git --git-dir " + srcDir + " archive " + tree + " took " + strconv.FormatFloat(duration, 'f', 5, 64) + "s")

			er = executeCommand(logCmd, config.Timeout, false)
			if len(er.output) > 0 {
				commitHash := strings.TrimSuffix(er.output, "\n")
				if strings.HasPrefix(srcDir, config.EnvCacheDir) {
					Debugf("Writing to deploy file " + deployFile)
					dr := DeployResult{
						Name:      tree,
						Signature: commitHash,
						StartedAt: startedAt,
					}
					writeStructJSONFile(deployFile, dr)
				} else {
					Debugf("Writing hash " + commitHash + " from command " + logCmd + " to " + hashFile)
					f, _ := os.Create(hashFile)
					defer f.Close()
					f.WriteString(commitHash)
					f.Sync()
				}

			}
		}
	}
	return true
}

func listGitRepoFiles(gitDir string, tree string, targetDir string, hashFile string) {
	treeCmd := "git --git-dir " + gitDir + " ls-tree --full-tree -r --name-only " + tree
	er := executeCommand(treeCmd, config.Timeout, false)
	foundGitFiles := strings.Split(er.output, "\n")
	mutex.Lock()
	// g10k must have purge whitelist items
	desiredContent = append(desiredContent, hashFile)
	desiredContent = append(desiredContent, ".last_commit")
	for _, desiredFile := range foundGitFiles[:len(foundGitFiles)-1] {
		desiredContent = append(desiredContent, filepath.Join(targetDir, desiredFile))

		// because we're using -r which prints git managed files in subfolders like this: foo/test3
		// we have to split up the given string and add the possible parent directories (foo in this case)
		parentDirs := strings.Split(desiredFile, "/")
		if len(parentDirs) > 1 {
			for _, dir := range parentDirs[:len(parentDirs)-1] {
				desiredContent = append(desiredContent, filepath.Join(targetDir, dir))
			}
		}
	}
	mutex.Unlock()
}
