package graph

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/engine"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/registry"
	"github.com/docker/docker/utils"
)

func (s *TagStore) CmdDiffAndApply(job *engine.Job) engine.Status {
	if n := len(job.Args); n != 3 {
		return job.Errorf("Usage : %s CONTAINERID SRCIMAGEID TAGIMAGEID", job.Name)
	}

	var (
		containerID   = job.Args[0]
		localName     = job.Args[1]
		parentImageID = job.Args[2]
		sf            = utils.NewStreamFormatter(job.GetenvBool("json"))
	)

	img, err := s.LookupImage(localName)
	if err != nil {
		return job.Error(err)
	}

	root := s.graph.Root
	root = strings.Replace(root, "graph", "", -1)
	dest := path.Join(root, "devicemapper", "mnt", containerID, "rootfs")

	fi, err := os.Stat(dest)
	if err != nil && !os.IsExist(err) {
		return job.Error(err)
	}
	if !fi.IsDir() {
		return job.Errorf(" Dest %s is not dir", dest)
	}

	job.Stdout.Write(sf.FormatProgress(utils.TruncateID(img.ID), fmt.Sprintf("Diff two mirrors(%s - %s)", img.ID, parentImageID), nil))
	fs, err := s.graph.Driver().Diff(img.ID, parentImageID)
	if err != nil {
		return job.Error(err)
	}
	defer fs.Close()
	job.Stdout.Write(sf.FormatProgress(utils.TruncateID(img.ID), "Complete", nil))

	job.Stdout.Write(sf.FormatProgress(utils.TruncateID(img.ID), fmt.Sprintf("Merge layer to container rootfs %s", dest), nil))
	err = archive.ApplyLayer(dest, fs)
	if err != nil {
		return job.Error(err)
	}

	job.Stdout.Write(sf.FormatProgress(utils.TruncateID(img.ID), "Complete", nil))
	return engine.StatusOK
}

func (s *TagStore) checkIsParent(id, parent string) (bool, error) {
	isParent := false
	img, err := s.graph.Get(id)
	if err != nil {
		return false, err
	}

	for {
		if img.Parent == "" {
			break
		}

		if img.Parent == parent {
			isParent = true
			break
		}

		img, err = s.graph.Get(img.Parent)
		if err != nil {
			break
		}
	}
	return isParent, err
}

func (s *TagStore) CmdPullAndApply(job *engine.Job) engine.Status {
	if n := len(job.Args); n != 4 {
		return job.Errorf("Usage: %s CONTAINERID CONTAINERIMAGE IMAGE TAG", job.Name)
	}

	var (
		containerID    = job.Args[0]
		containerImage = job.Args[1]
		localName      = job.Args[2]
		tag            = job.Args[3]
		sf             = utils.NewStreamFormatter(job.GetenvBool("json"))
		authConfig     = &registry.AuthConfig{}
		metaHeaders    map[string][]string
		mirrors        []string
	)

	job.GetenvJson("authConfig", authConfig)
	job.GetenvJson("metaHeaders", &metaHeaders)

	for {
		c, err := s.poolAdd("pull", localName+":"+tag)
		if err != nil {
			if c != nil {
				// Another pull of the same repository is already taking place; just wait for it to finish
				job.Stdout.Write(sf.FormatStatus("", "Repository %s already being pulled by another client. Waiting.", localName))
				<-c
				continue
			} else {
				return job.Error(err)
			}
		}
		break
	}
	defer s.poolRemove("pull", localName+":"+tag)

	// Resolve the Repository name from fqn to endpoint + name
	hostname, remoteName, err := registry.ResolveRepositoryName(localName)
	if err != nil {
		return job.Error(err)
	}

	endpoint, err := registry.NewEndpoint(hostname, s.insecureRegistries)
	if err != nil {
		return job.Error(err)
	}

	r, err := registry.NewSession(authConfig, registry.HTTPRequestFactory(metaHeaders), endpoint, true)
	if err != nil {
		return job.Error(err)
	}

	if endpoint.VersionString(1) == registry.IndexServerAddress() {
		// If pull "index.docker.io/foo/bar", it's stored locally under "foo/bar"
		localName = remoteName
		isOfficial := isOfficialName(remoteName)
		if isOfficial && strings.IndexRune(remoteName, '/') == -1 {
			remoteName = "library/" + remoteName
		}
	}
	// Use provided mirrors, if any
	mirrors = s.mirrors

	if err = s.pullMRepository(r, job.Stdout, containerID, containerImage, localName, remoteName, tag, sf, job.GetenvBool("parallel"), mirrors); err != nil {
		return job.Error(err)
	}

	return engine.StatusOK
}

func (s *TagStore) pullMRepository(r *registry.Session, out io.Writer, containerID, containerImage, localName, remoteName, askedTag string, sf *utils.StreamFormatter, parallel bool, mirrors []string) error {
	out.Write(sf.FormatStatus("", "Pulling (parallel:%v) repository %s:%s", parallel, localName, askedTag))

	if askedTag == "" {
		return fmt.Errorf("Error: tag is empty")
	}

	repoData, err := r.GetRepositoryData(remoteName)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP code: 404") {
			return fmt.Errorf("Error: image %s not found", remoteName)
		}
		// Unexpected HTTP error
		return err
	}

	log.Debugf("Retrieving the tag list")
	tagsList, err := r.GetRemoteTags(repoData.Endpoints, remoteName, repoData.Tokens)
	if err != nil {
		log.Errorf("%v", err)
		return err
	}

	id, exists := tagsList[askedTag]
	if !exists {
		return fmt.Errorf("Tag %s not found in repository %s", askedTag, localName)
	}
	img, exists := repoData.ImgList[id]
	if !exists {
		return fmt.Errorf("Image ID %s not found in repository %s", id, localName)
	}
	img.Tag = askedTag

	var lastErr error
	success := false
	var is_downloaded bool
	if mirrors != nil {
		for _, ep := range mirrors {
			out.Write(sf.FormatProgress(utils.TruncateID(img.ID), fmt.Sprintf("Pulling and merging image (%s) from %s, mirror: %s", img.Tag, localName, ep), nil))
			if is_downloaded, err = s.pullAndMergeImage(r, out, containerID, containerImage, img.ID, ep, repoData.Tokens, sf); err != nil {
				// Don't report errors when pulling from mirrors.
				log.Debugf("Error pulling and merging image (%s) from %s, mirror: %s, %s", img.Tag, localName, ep, err)
				continue
			}
			success = true
			break
		}
	}
	if !success {
		for _, ep := range repoData.Endpoints {
			out.Write(sf.FormatProgress(utils.TruncateID(img.ID), fmt.Sprintf("Pulling and merging image (%s) from %s, endpoint: %s", img.Tag, localName, ep), nil))
			if is_downloaded, err = s.pullAndMergeImage(r, out, containerID, containerImage, img.ID, ep, repoData.Tokens, sf); err != nil {
				// It's not ideal that only the last error is returned, it would be better to concatenate the errors.
				// As the error is also given to the output stream the user will see the error.
				lastErr = err
				out.Write(sf.FormatProgress(utils.TruncateID(img.ID), fmt.Sprintf("Error pulling and merging image (%s) from %s, endpoint: %s, %s", img.Tag, localName, ep, err), nil))
				continue
			}
			success = true
			break
		}
	}
	if !success {
		err := fmt.Errorf("Error pulling and merging image (%s) from %s, %v", img.Tag, localName, lastErr)
		out.Write(sf.FormatProgress(utils.TruncateID(img.ID), err.Error(), nil))
		return err
	}
	out.Write(sf.FormatProgress(utils.TruncateID(img.ID), fmt.Sprintf("Layer is downloaded ? %v", is_downloaded), nil))
	out.Write(sf.FormatProgress(utils.TruncateID(img.ID), "Complete", nil))

	if err = s.Set(localName, img.Tag, img.ID, true); err != nil {
		return err
	}

	return nil
}

func (s *TagStore) pullAndMergeImage(r *registry.Session, out io.Writer, containerID, containerImage, imgID, endpoint string, token []string, sf *utils.StreamFormatter) (bool, error) {
	newHistory, err := r.GetRemoteHistory(imgID, endpoint, token)
	if err != nil {
		return false, err
	}
	oldHistory, err := r.GetRemoteHistory(containerImage, endpoint, token)
	if err != nil {
		return false, err
	}
	// Compare the differences between the two image
	compareHistory := make(map[string]string, len(oldHistory))
	for _, id := range oldHistory {
		compareHistory[id] = id
	}
	var history []string
	for _, id := range newHistory {
		if _, ok := compareHistory[id]; !ok {
			history = append(history, id)
		}
	}

	layers_downloaded := false
	for i := len(history) - 1; i >= 0; i-- {
		id := history[i]

		// ensure no two downloads of the same layer happen at the same time
		if c, err := s.poolAdd("pull", "layer:"+id); err != nil {
			log.Debugf("Image (id: %s) pull is already running, skipping: %v", id, err)
			<-c
		}
		defer s.poolRemove("pull", "layer:"+id)

		out.Write(sf.FormatProgress(utils.TruncateID(id), "Pulling metadata", nil))
		var (
			imgJSON []byte
			imgSize int
			err     error
			img     *image.Image
		)
		retries := 5
		for j := 1; j <= retries; j++ {
			imgJSON, imgSize, err = r.GetRemoteImageJSON(id, endpoint, token)
			if err != nil && j == retries {
				out.Write(sf.FormatProgress(utils.TruncateID(id), "Error pulling dependent layers", nil))
				return layers_downloaded, err
			} else if err != nil {
				time.Sleep(time.Duration(j) * 500 * time.Millisecond)
				continue
			}
			img, err = image.NewImgJSON(imgJSON)
			layers_downloaded = true
			if err != nil && j == retries {
				out.Write(sf.FormatProgress(utils.TruncateID(id), "Error pulling dependent layers", nil))
				return layers_downloaded, fmt.Errorf("Failed to parse json: %s", err)
			} else if err != nil {
				time.Sleep(time.Duration(j) * 500 * time.Millisecond)
				continue
			} else {
				break
			}
		}

		for j := 1; j <= retries; j++ {
			// Get the layer
			status := "Pulling fs layer"
			if j > 1 {
				status = fmt.Sprintf("Pulling fs layer [retries: %d]", j)
			}
			out.Write(sf.FormatProgress(utils.TruncateID(id), status, nil))
			layer, err := r.GetRemoteImageLayer(img.ID, endpoint, token, int64(imgSize))
			if uerr, ok := err.(*url.Error); ok {
				err = uerr.Err
			}
			if terr, ok := err.(net.Error); ok && terr.Timeout() && j < retries {
				time.Sleep(time.Duration(j) * 500 * time.Millisecond)
				continue
			} else if err != nil {
				out.Write(sf.FormatProgress(utils.TruncateID(id), "Error pulling dependent layers", nil))
				return layers_downloaded, err
			}
			layers_downloaded = true
			defer layer.Close()
			if !s.graph.Exists(id) {
				// register when first pull layer
				err = s.graph.Register(img, imgJSON,
					utils.ProgressReader(layer, imgSize, out, sf, false, utils.TruncateID(id), "Downloading"))
				if terr, ok := err.(net.Error); ok && terr.Timeout() && j < retries {
					time.Sleep(time.Duration(j) * 500 * time.Millisecond)
					continue
				} else if err != nil {
					out.Write(sf.FormatProgress(utils.TruncateID(id), "Error downloading dependent layers", nil))
					return layers_downloaded, err
				}
			}
			// add layer to container
			root := s.graph.Root
			root = strings.Replace(root, "graph", "", -1)
			dest := path.Join(root, "devicemapper", "mnt", containerID, "rootfs")
			out.Write(sf.FormatProgress(utils.TruncateID(id), fmt.Sprintf("Merge layer to container rootfs %s", dest), nil))
			err = archive.ApplyLayer(dest, layer)
			if err != nil && j == retries {
				out.Write(sf.FormatProgress(utils.TruncateID(id), "Error merge layers", nil))
				return layers_downloaded, err
			} else if err != nil {
				time.Sleep(time.Duration(j) * 500 * time.Millisecond)
				continue
			} else {
				break
			}
		}
	}
	return layers_downloaded, nil
}
