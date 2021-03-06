package cephdriver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// CephVolume is a struct that communicates volume name and size.
type CephVolume struct {
	VolumeName string // Name of the volume
	PoolName   string
	VolumeSize uint64 // Size in MBs
	driver     *CephDriver
}

// CephMount is an informational struct returned by the Mount call to yield
// data on the mounted object.
type CephMount struct {
	DevMajor   uint
	DevMinor   uint
	DeviceName string
	MountPath  string
}

func (cv *CephVolume) String() string {
	return fmt.Sprintf("[name: %s/%s size: %d]", cv.PoolName, cv.VolumeName, cv.VolumeSize)
}

// Exists returns true if the volume already exists.
func (cv *CephVolume) Exists() (bool, error) {
	out, err := exec.Command("rbd", "ls", cv.PoolName).Output()
	if err != nil {
		return false, err
	}

	list := strings.Split(string(out), "\n")

	for _, volName := range list {
		if volName == cv.VolumeName {
			return true, nil
		}
	}

	return false, nil
}

// Create creates an RBD image and initialize ext4 filesystem on the image
func (cv *CephVolume) Create(fscmd string) error {
	ok, err := cv.driver.PoolExists(cv.PoolName)
	if err != nil {
		return err
	}

	if !ok {
		return fmt.Errorf("Pool %q does not exist", cv.PoolName)
	}

	if ok, err := cv.Exists(); ok && err == nil {
		return nil
	} else if err != nil {
		return err
	}

	if err := cv.volumeCreate(); err != nil {
		return err
	}

	blkdev, err := cv.mapImage()
	if err != nil {
		return err
	}

	if err := cv.driver.mkfsVolume(fscmd, blkdev); err != nil {
		return err
	}

	if err := cv.unmapImage(); err != nil {
		return err
	}

	return nil
}

// Mount maps an RBD image and mount it on /mnt/ceph/<datastore>/<volume> directory
// FIXME: Figure out how to use rbd locks
func (cv *CephVolume) Mount(fstype string) (*CephMount, error) {
	cd := cv.driver
	// Directory to mount the volume
	dataStoreDir := filepath.Join(cd.mountBase, cv.PoolName)
	volumeDir := filepath.Join(dataStoreDir, cv.VolumeName)

	devName, err := cv.mapImage()
	if err != nil {
		return nil, err
	}

	// Create directory to mount
	if err := os.MkdirAll(cd.mountBase, 0700); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("error creating %q directory: %v", cd.mountBase, err)
	}

	if err := os.MkdirAll(volumeDir, 0700); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("error creating %q directory: %v", dataStoreDir)
	}

	// Obtain the major and minor node information about the device we're mounting.
	// This is critical for tuning cgroups and obtaining metrics for this device only.
	fi, err := os.Stat(devName)
	if err != nil {
		return nil, fmt.Errorf("Failed to stat rbd device %q: %v", devName, err)
	}

	rdev := fi.Sys().(*syscall.Stat_t).Rdev

	major := rdev >> 8
	minor := rdev & 0xFF

	// Mount the RBD
	if err := unix.Mount(devName, volumeDir, fstype, 0, ""); err != nil && err != unix.EBUSY {
		return nil, fmt.Errorf("Failed to mount RBD dev %q: %v", devName, err.Error())
	}

	return &CephMount{
		DeviceName: devName,
		MountPath:  volumeDir,
		DevMajor:   uint(major),
		DevMinor:   uint(minor),
	}, nil
}

// Unmount unmounts a Ceph volume, remove the mount directory and unmap
// the RBD device
func (cv *CephVolume) Unmount() error {
	cd := cv.driver

	// formatted image name
	// Directory to mount the volume
	dataStoreDir := filepath.Join(cd.mountBase, cv.PoolName)
	volumeDir := filepath.Join(dataStoreDir, cv.VolumeName)

	// Unmount the RBD
	//
	// MNT_DETACH will make this mountpoint unavailable to new open file requests (at
	// least until it is remounted) but persist for existing open requests. This
	// seems to work well with containers.
	//
	// The checks for ENOENT and EBUSY below are safeguards to prevent error
	// modes where multiple containers will be affecting a single volume.
	// FIXME loop over unmount and ensure the unmount finished before removing dir
	if err := unix.Unmount(volumeDir, unix.MNT_DETACH); err != nil && err != unix.ENOENT {
		return fmt.Errorf("Failed to unmount %q: %v", volumeDir, err)
	}

	// Remove the mounted directory
	// FIXME remove all, but only after the FIXME above.
	if err := os.Remove(volumeDir); err != nil && !os.IsNotExist(err) {
		if err, ok := err.(*os.PathError); ok && err.Err == unix.EBUSY {
			return nil
		}

		return fmt.Errorf("error removing %q directory: %v", volumeDir, err)
	}

	if err := cv.unmapImage(); err != os.ErrNotExist {
		return err
	}

	return nil
}

// Remove removes an RBD volume i.e. rbd image, and its snapshots
func (cv *CephVolume) Remove() error {
	if err := exec.Command("rbd", "snap", "purge", cv.VolumeName, "--pool", cv.PoolName).Run(); err != nil {
		return err
	}

	return exec.Command("rbd", "rm", cv.VolumeName, "--pool", cv.PoolName).Run()
}

// CreateSnapshot creates a named snapshot for the volume. Any error will be returned.
func (cv *CephVolume) CreateSnapshot(snapName string) error {
	snapName = strings.Replace(snapName, " ", "-", -1)
	return exec.Command("rbd", "snap", "create", cv.VolumeName, "--snap", snapName, "--pool", cv.PoolName).Run()
}

// RemoveSnapshot removes a named snapshot for the volume. Any error will be returned.
func (cv *CephVolume) RemoveSnapshot(snapName string) error {
	return exec.Command("rbd", "snap", "rm", cv.VolumeName, "--snap", snapName, "--pool", cv.PoolName).Run()
}

// ListSnapshots returns an array of snapshot names provided a maximum number
// of snapshots to be returned. Any error will be returned.
func (cv *CephVolume) ListSnapshots() ([]string, error) {
	out, err := exec.Command("rbd", "snap", "ls", cv.VolumeName, "--pool", cv.PoolName).Output()
	if err != nil {
		return nil, err
	}

	names := []string{}

	lines := strings.Split(string(out), "\n")
	if len(lines) > 1 {
		for _, line := range lines[1:] {
			parts := regexp.MustCompile(`\s+`).Split(line, -1)
			if len(parts) < 3 {
				continue
			}

			names = append(names, parts[2])
		}
	}

	return names, nil
}
