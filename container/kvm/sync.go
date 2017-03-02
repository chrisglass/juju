// Copyright 2016 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package kvm

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"

	"github.com/juju/errors"
	"github.com/juju/utils/series"

	"github.com/juju/juju/environs/imagedownloads"
	"github.com/juju/juju/environs/simplestreams"
	"github.com/juju/juju/juju/paths"
)

// BIOSFType is the file type we want to fetch and use for kvm instances which
// boot using a legacy BIOS boot loader.
const BIOSFType = "disk1.img"

// UEFIFType is teh file type we want to fetch and use for kvm instances which
// boot using UEFI. In our case this is ARM64.
const UEFIFType = "uefi1.img"

// Oner gets the one matching item from simplestreams.
type Oner interface {
	One() (*imagedownloads.Metadata, error)
}

// syncParams conveys the information necessary for calling imagedownloads.One.
type syncParams struct {
	arch, series, ftype string
	srcFunc             func() simplestreams.DataSource
}

// One implements Oner.
func (p syncParams) One() (*imagedownloads.Metadata, error) {
	if err := p.exists(); err != nil {
		return nil, errors.Trace(err)
	}
	return imagedownloads.One(p.arch, p.series, p.ftype, p.srcFunc)
}

func (p syncParams) exists() error {
	fname := backingFileName(p.series, p.arch)
	baseDir, err := paths.DataDir(series.MustHostSeries())
	if err != nil {
		return errors.Trace(err)
	}
	path := filepath.Join(baseDir, kvm, guestDir, fname)

	if _, err := os.Stat(path); err == nil {
		return errors.AlreadyExistsf("%q %q image for exists at %q", p.series, p.arch, path)
	}
	return nil
}

// Validate that our types fulfull their implementations.
var _ Oner = (*syncParams)(nil)
var _ Fetcher = (*fetcher)(nil)

// Fetcher is an interface to permit faking input in tests. The default
// implementation is updater, defined in this file.
type Fetcher interface {
	Fetch() error
	Close()
}

type fetcher struct {
	metadata *imagedownloads.Metadata
	req      *http.Request
	client   *http.Client
	image    *Image
}

// Fetch implements Fetcher. It fetches the image file from simplestreams and
// delegates writing it out and creating the qcow3 backing file to Image.write.
func (f *fetcher) Fetch() error {
	resp, err := f.client.Do(f.req)
	if err != nil {
		return errors.Trace(err)
	}

	defer func() {
		err = resp.Body.Close()
		if err != nil {
			logger.Debugf("failed defer %q", errors.Trace(err))
		}
	}()

	if resp.StatusCode != 200 {
		f.image.cleanup()
		return errors.NotFoundf(
			"got %d fetching image %q", resp.StatusCode, path.Base(
				f.req.URL.String()))
	}
	err = f.image.write(resp.Body, f.metadata)
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

// Close calls images cleanup method for deferred closing of the image tmpFile.
func (f *fetcher) Close() {
	f.image.cleanup()
}

// Sync updates the local cached images by reading the simplestreams data and
// caching if an image matching the contrainsts doesn't exist. It retrieves
// metadata information from Oner and updates local cache via Fetcher.
func Sync(o Oner, f Fetcher) error {
	md, err := o.One()
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// We've already got a backing file for this series/architecture.
			return nil
		}
		return errors.Trace(err)
	}
	if f == nil {
		f, err = newDefaultFetcher(md, paths.DataDir)
		if err != nil {
			return errors.Trace(err)
		}
		defer f.Close()
	}
	err = f.Fetch()
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

// Image represents a server image.
type Image struct {
	FilePath string
	tmpFile  *os.File
	runCmd   runFunc
}

// write saves the stream to disk and updates the metadata file.
func (i *Image) write(r io.Reader, md *imagedownloads.Metadata) error {
	tmpPath := i.tmpFile.Name()
	defer func() {
		err := i.tmpFile.Close()
		if err != nil {
			logger.Errorf("failed to close %q %s", tmpPath, err)
		}
		err = os.Remove(tmpPath)
		if err != nil {
			logger.Errorf("failed to remove %q after use %s", tmpPath, err)
		}

	}()

	hash := sha256.New()
	_, err := io.Copy(io.MultiWriter(i.tmpFile, hash), r)
	if err != nil {
		i.cleanup()
		return errors.Trace(err)
	}

	result := fmt.Sprintf("%x", hash.Sum(nil))
	if result != md.SHA256 {
		i.cleanup()
		return errors.Errorf(
			"hash sum mismatch for %s: %s != %s", i.tmpFile.Name(), result, md.SHA256)
	}

	output, err := i.runCmd(
		"qemu-img", "convert", "-f", "qcow2", tmpPath, i.FilePath)
	logger.Debugf("qemu-image convert output: %s", output)
	if err != nil {
		i.cleanupAll()
		return errors.Trace(err)
	}
	return nil
}

// cleanup attempts to close and remove the tempfile download image. It can be
// called if things don't work out. E.g. sha256 mismatch, incorrect size...
func (i *Image) cleanup() {
	if err := i.tmpFile.Close(); err != nil {
		logger.Debugf("%s", err.Error())
	}

	if err := os.Remove(i.tmpFile.Name()); err != nil {
		logger.Debugf("got %q removing %q", err.Error(), i.tmpFile.Name())
	}
}

// cleanupAll cleans up the possible backing file as well.
func (i *Image) cleanupAll() {
	i.cleanup()
	err := os.Remove(i.FilePath)
	if err != nil {
		logger.Debugf("got %q removing %q", err.Error(), i.FilePath)
	}
}

func newDefaultFetcher(md *imagedownloads.Metadata, pathfinder func(string) (string, error)) (*fetcher, error) {
	i, err := newImage(md, pathfinder)
	if err != nil {
		return nil, errors.Trace(err)
	}
	dlURL, err := md.DownloadURL()
	if err != nil {
		return nil, errors.Trace(err)
	}
	req, err := http.NewRequest("GET", dlURL.String(), nil)
	if err != nil {
		return nil, errors.Trace(err)
	}
	client := &http.Client{}
	return &fetcher{metadata: md, image: i, client: client, req: req}, nil
}

func newImage(md *imagedownloads.Metadata, pathfinder func(string) (string, error)) (*Image, error) {
	// Setup names and paths.
	dlURL, err := md.DownloadURL()
	if err != nil {
		return nil, errors.Trace(err)
	}
	baseDir, err := pathfinder(series.MustHostSeries())
	if err != nil {
		return nil, errors.Trace(err)
	}

	// Closing this is deferred in Image.write.
	fh, err := ioutil.TempFile("", fmt.Sprintf("juju-kvm-%s-", path.Base(dlURL.String())))
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &Image{
		FilePath: filepath.Join(
			baseDir, kvm, guestDir, backingFileName(md.Release, md.Arch)),
		tmpFile: fh,
		runCmd:  run,
	}, nil
}

func backingFileName(series, arch string) string {
	// TODO(ro) validate series and arch to be sure they are in the right order.
	return fmt.Sprintf("%s-%s-backing-file.qcow", series, arch)
}