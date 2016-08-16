package repobuilder

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"text/template"

	"github.com/mongodb/amboy"
	"github.com/mongodb/amboy/job"
	"github.com/mongodb/amboy/registry"
	"github.com/mongodb/curator"
	"github.com/pkg/errors"
	"github.com/tychoish/grip"
)

const releaseMetaDataDateFormat = "Wed, 15 Jun 2006 00:02:52 UTC"

type BuildDEBRepoJob struct {
	*Job
}

func init() {
	registry.AddJobType("build-deb-repo", func() amboy.Job {
		return &BuildDEBRepoJob{buildRepoJob()}
	})
}

// TODO: need to find some way to create the arch directories if they
// don't exist, which probably means a configuration change. Currently
// arch directories are created when attempting to build the repos,
// which means that there's a condition where we add a new
// architecture (e.g. a community arm build,) and until we've pushed
// packages to all repos the repo-metadata will be "ahead" of the
// state of the repo. Should correct itself when everything
// pushes. Unclear if current solution is susceptible to this.

func NewBuildDEBRepo(conf *RepositoryConfig, distro *RepositoryDefinition, version, arch, profile string, pkgs ...string) (*BuildDEBRepoJob, error) {
	var err error
	r := &BuildDEBRepoJob{Job: buildRepoJob()}

	r.release, err = curator.NewMongoDBVersion(version)
	if err != nil {
		return nil, err
	}

	r.WorkSpace, err = os.Getwd()
	if err != nil {
		grip.Errorln("system error: cannot determine the current working directory.",
			"not creating a job object.")
		return nil, err
	}

	r.JobType = amboy.JobType{
		Name:    "build-deb-repo",
		Version: 0,
	}

	if arch == "x86_64" {
		r.Arch = "amd64"
	} else if arch == "ppc64le" {
		r.Arch = "ppc64el"
	} else {
		r.Arch = arch
	}

	r.Name = fmt.Sprintf("build-deb-repo.%d", job.GetNumber())
	r.Distro = distro
	r.Conf = conf
	r.PackagePaths = pkgs
	r.Version = version
	r.Profile = profile
	return r, nil
}

func (j *BuildDEBRepoJob) createArchDirs(basePath string) error {
	catcher := grip.NewCatcher()

	for _, arch := range j.Distro.Architectures {
		path := filepath.Join(basePath, "binary-"+arch)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			err = os.MkdirAll(path, 0755)
			if err != nil {
				catcher.Add(err)
				continue
			}

			// touch the Packages file
			err = ioutil.WriteFile(filepath.Join(path, "Packages"), []byte(""), 0644)
			if err != nil {
				catcher.Add(err)
				continue
			}

			catcher.Add(gzipAndWriteToFile(filepath.Join(path, "Packages.gz"), []byte("")))
		}
	}

	return catcher.Resolve()
}

func (j *BuildDEBRepoJob) injectPackage(local, repoName string) (string, error) {
	catcher := grip.NewCatcher()

	repoPath := filepath.Join(local, repoName, j.Distro.Component)
	err := j.createArchDirs(repoPath)
	catcher.Add(j.linkPackages(filepath.Join(repoPath, "binary-"+j.Arch)))
	catcher.Add(err)

	return repoPath, catcher.Resolve()
}

func gzipAndWriteToFile(fileName string, content []byte) error {
	var gz bytes.Buffer

	w, err := gzip.NewWriterLevel(&gz, flate.BestCompression)
	if err != nil {
		return errors.Wrapf(err, "compressing file '%s'", fileName)
	}

	_, err = w.Write(content)
	if err != nil {
		return errors.Wrapf(err, "writing content '%s", fileName)
	}
	err = w.Close()
	if err != nil {
		return errors.Wrapf(err, "closing buffer '%s", fileName)
	}

	err = ioutil.WriteFile(fileName, gz.Bytes(), 0644)
	if err != nil {
		return errors.Wrapf(err, "writing compressed file '%s'", fileName)
	}

	grip.Noticeln("wrote zipped packages file to:", fileName)
	return nil
}

func (j *BuildDEBRepoJob) rebuildRepo(workingDir string, wg *sync.WaitGroup) {
	defer wg.Done()

	arch := "binary-" + j.Arch

	// start by running dpkg-scanpackages to generate a packages file
	// in the source.
	dirParts := strings.Split(workingDir, string(filepath.Separator))
	cmd := exec.Command("dpkg-scanpackages", "--multiversion", filepath.Join(filepath.Join(dirParts[len(dirParts)-5:]...), arch))
	cmd.Dir = string(filepath.Separator) + filepath.Join(dirParts[:len(dirParts)-5]...)

	grip.Infof("running command='%s' path='%s'", strings.Join(cmd.Args, " "), cmd.Dir)
	out, err := cmd.Output()
	if err != nil {
		j.addError(errors.Wrapf(err, "building 'Packages': [%s]", string(out)))
		return
	}

	// Write the packages file to disk.
	pkgsFile := filepath.Join(workingDir, arch, "Packages")
	err = ioutil.WriteFile(pkgsFile, out, 0644)
	if err != nil {
		j.addError(err)
		return
	}
	grip.Noticeln("wrote packages file to:", pkgsFile)

	// Compress/gzip the packages file
	err = gzipAndWriteToFile(pkgsFile+".gz", out)
	if err != nil {
		j.addError(errors.Wrap(err, "compressing the 'Packages' file"))
		return
	}

	// Continue by building the Releases file, first by using the
	// template, and then by
	releaseTmplSrc, ok := j.Conf.Templates.Deb[j.Distro.Edition]
	if !ok {
		j.addError(errors.Errorf("no 'Release' template defined for %s", j.Distro.Edition))
		return
	}

	// initialize the template.
	tmpl, err := template.New("Releases").Parse(releaseTmplSrc)
	if err != nil {
		j.addError(errors.Wrap(err, "reading Releases template"))
		return
	}

	buffer := bytes.NewBuffer([]byte{})
	err = tmpl.Execute(buffer, struct {
		CodeName      string
		Component     string
		Architectures string
	}{
		CodeName:      j.Distro.CodeName,
		Component:     j.Distro.Component,
		Architectures: strings.Join(j.Distro.Architectures, " "),
	})
	if err != nil {
		j.addError(errors.Wrap(err, "rendering Releases template"))
		return
	}

	// This builds a Release file using the header info generated
	// from the template above.
	cmd = exec.Command("apt-ftparchive", "release", "../")
	cmd.Dir = workingDir
	out, err = cmd.Output()
	grip.Infof("generating release file: [command='%s', path='%s']", strings.Join(cmd.Args, " "), cmd.Dir)
	outString := string(out)
	grip.Debug(outString)
	if err != nil {
		j.addError(errors.Wrapf(err, "generating Release content for %s", workingDir))
		return
	}

	// get the content from the template and add the output of
	// apt-ftparchive there.
	releaseContent := buffer.Bytes()
	releaseContent = append(releaseContent, out...)

	// tracking the output is useful. we'll do that here.
	j.mutex.Lock()
	j.Output["sign-release-file-"+workingDir] = outString
	j.mutex.Unlock()

	// write the content of the release file to disk.
	relFileName := filepath.Join(filepath.Dir(workingDir), "Release")
	err = ioutil.WriteFile(relFileName, releaseContent, 0644)
	if err != nil {
		j.addError(errors.Wrapf(err, "writing Release file to disk %s", relFileName))
		return
	}

	grip.Noticeln("wrote release files to:", relFileName)

	// sign the file using the notary service. To remove the
	// MongoDB-specificity we could make this configurable, or
	// offer ways of specifying different signing option.
	err = j.signFile(relFileName, "gpg", false) // (name, extension, overwrite)
	if err != nil {
		j.addError(errors.Wrapf(err, "signing Release file for %s", workingDir))
		return
	}

	// build the index page.
	err = j.Conf.BuildIndexPageForDirectory(workingDir, j.Distro.Bucket)
	if err != nil {
		j.addError(errors.Wrapf(err, "building index.html pages for %s", workingDir))
		return
	}
}
