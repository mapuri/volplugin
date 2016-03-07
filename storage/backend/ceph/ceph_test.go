package ceph

import (
	"io"
	"os"
	"strings"
	. "testing"
	"time"

	. "gopkg.in/check.v1"

	log "github.com/Sirupsen/logrus"
	"github.com/contiv/volplugin/storage"
)

const myMountpath = "/mnt/ceph"

var filesystems = map[string]storage.FSOptions{
	"ext4": {
		Type:          "ext4",
		CreateCommand: "mkfs.ext4 -m0 %",
	},
}

var volumeSpec = storage.Volume{
	Name:   "pithos",
	Size:   10,
	Params: storage.Params{"pool": "rbd"},
}

type cephSuite struct{}

var _ = Suite(&cephSuite{})

func TestCeph(t *T) { TestingT(t) }

func (s *cephSuite) SetUpTest(c *C) {
	if os.Getenv("DEBUG") != "" {
		log.SetLevel(log.DebugLevel)
	}
}

func (s *cephSuite) readWriteTest(c *C, mountDir string) {
	// Write a file and verify you can read it
	file, err := os.Create(mountDir + "/test.txt")
	c.Assert(err, IsNil)

	_, err = file.WriteString("Test string\n")
	c.Assert(err, IsNil)

	file.Close()

	file, err = os.Open(mountDir + "/test.txt")
	c.Assert(err, IsNil)

	rb := make([]byte, 11)
	_, err = io.ReadAtLeast(file, rb, 11)
	c.Assert(err, IsNil)

	file.Close()

	var rbs = strings.TrimSpace(string(rb))
	c.Assert(rbs, Equals, strings.TrimSpace("Test string"))
}

func (s *cephSuite) TestMkfsVolume(c *C) {
	// Create a new driver; the ceph driver is needed
	driver := Driver{mountpath: myMountpath}

	err := driver.mkfsVolume("echo %s; sleep 1", "fake-fake-fake", 3*time.Second)
	c.Assert(err, IsNil)

	err = driver.mkfsVolume("echo %s; sleep 2", "fake-fake-fake", 1*time.Second)
	c.Assert(err, NotNil)
}

func (s *cephSuite) TestMountUnmountVolume(c *C) {
	// Create a new driver
	driver := NewDriver(myMountpath)

	driverOpts := storage.DriverOptions{
		Volume:    volumeSpec,
		FSOptions: filesystems["ext4"],
	}

	// we don't care if there's an error here, just want to make sure the create
	// succeeds. Easier restart of failed tests this way.
	driver.Unmount(driverOpts)
	driver.Destroy(driverOpts)

	c.Assert(driver.Create(driverOpts), IsNil)
	defer driver.Destroy(driverOpts)
	c.Assert(driver.Format(driverOpts), IsNil)
	ms, err := driver.Mount(driverOpts)
	c.Assert(err, IsNil)
	c.Assert(ms.Volume, DeepEquals, volumeSpec)
	c.Assert(ms.DevMajor, Equals, uint(252))
	c.Assert(ms.DevMinor, Equals, uint(0))
	c.Assert(strings.HasPrefix(ms.Device, "/dev/rbd"), Equals, true)
	s.readWriteTest(c, driver.MountPath(driverOpts))
	c.Assert(driver.Unmount(driverOpts), IsNil)
	c.Assert(driver.Destroy(driverOpts), IsNil)
}

func (s *cephSuite) TestSnapshots(c *C) {
	driver := NewDriver(myMountpath)
	driverOpts := storage.DriverOptions{
		Volume:    volumeSpec,
		FSOptions: filesystems["ext4"],
	}

	c.Assert(driver.Create(driverOpts), IsNil)
	defer driver.Destroy(driverOpts)
	c.Assert(driver.CreateSnapshot("hello", driverOpts), IsNil)
	c.Assert(driver.CreateSnapshot("hello", driverOpts), NotNil)

	list, err := driver.ListSnapshots(driverOpts)
	c.Assert(err, IsNil)
	c.Assert(len(list), Equals, 1)
	c.Assert(list, DeepEquals, []string{"hello"})

	c.Assert(driver.RemoveSnapshot("hello", driverOpts), IsNil)
	c.Assert(driver.RemoveSnapshot("hello", driverOpts), NotNil)

	list, err = driver.ListSnapshots(driverOpts)
	c.Assert(err, IsNil)
	c.Assert(len(list), Equals, 0)
	c.Assert(driver.Destroy(driverOpts), IsNil)
}

func (s *cephSuite) TestRepeatedMountUnmount(c *C) {
	driver := NewDriver(myMountpath)
	driverOpts := storage.DriverOptions{
		Volume:    volumeSpec,
		FSOptions: filesystems["ext4"],
	}

	// we don't care if there's an error here, just want to make sure the create
	// succeeds. Easier restart of failed tests this way.
	driver.Unmount(driverOpts)
	driver.Destroy(driverOpts)

	defer driver.Unmount(driverOpts)
	defer driver.Destroy(driverOpts)

	c.Assert(driver.Create(driverOpts), IsNil)
	c.Assert(driver.Format(driverOpts), IsNil)
	for i := 0; i < 10; i++ {
		_, err := driver.Mount(driverOpts)
		c.Assert(err, IsNil)
		s.readWriteTest(c, "/mnt/ceph/rbd/pithos")
		c.Assert(driver.Unmount(driverOpts), IsNil)
	}
	c.Assert(driver.Destroy(driverOpts), IsNil)
}

func (s *cephSuite) TestTemplateFSCmd(c *C) {
	c.Assert(templateFSCmd("%", "foo"), Equals, "foo")
	c.Assert(templateFSCmd("%%", "foo"), Equals, "%%")
	c.Assert(templateFSCmd("%%%", "foo"), Equals, "%%foo")
	c.Assert(templateFSCmd("% test % test %", "foo"), Equals, "foo test foo test foo")
	c.Assert(templateFSCmd("% %% %", "foo"), Equals, "foo %% foo")
	c.Assert(templateFSCmd("mkfs.ext4 -m0 %", "/dev/sda1"), Equals, "mkfs.ext4 -m0 /dev/sda1")
}

func (s *cephSuite) TestMounted(c *C) {
	driver := NewDriver(myMountpath)
	driverOpts := storage.DriverOptions{
		Volume:    volumeSpec,
		FSOptions: filesystems["ext4"],
		Timeout:   2 * time.Minute,
	}

	// we don't care if there's an error here, just want to make sure the create
	// succeeds. Easier restart of failed tests this way.
	driver.Unmount(driverOpts)
	driver.Destroy(driverOpts)

	c.Assert(driver.Create(driverOpts), IsNil)
	c.Assert(driver.Format(driverOpts), IsNil)
	_, err := driver.Mount(driverOpts)
	c.Assert(err, IsNil)
	mounts, err := driver.Mounted(2 * time.Minute)
	c.Assert(err, IsNil)

	c.Assert(mounts, DeepEquals, []*storage.Mount{
		{
			Device:   "/dev/rbd0",
			DevMajor: 252,
			DevMinor: 0,
			Path:     strings.Join([]string{myMountpath, volumeSpec.Params["pool"], volumeSpec.Name}, "/"),
			Volume: storage.Volume{
				Name: "pithos",
				Params: map[string]string{
					"pool": "rbd",
				},
			},
		},
	})

	c.Assert(driver.Unmount(driverOpts), IsNil)
	c.Assert(driver.Destroy(driverOpts), IsNil)
}

func (s *cephSuite) TestInternalNames(c *C) {
	driver := NewDriver(myMountpath)
	out, err := driver.InternalName("tenant1/test")
	c.Assert(err, IsNil)
	c.Assert(out, Equals, "tenant1.test")

	out, err = driver.InternalName("tenant1.test/test")
	c.Assert(err, NotNil)
	c.Assert(out, Equals, "")

	out, err = driver.InternalName("tenant1/test.two")
	c.Assert(err, IsNil)
	c.Assert(out, Equals, "tenant1.test.two")

	out, err = driver.InternalName("tenant1/test/two")
	c.Assert(err, NotNil)
	c.Assert(out, Equals, "")

	c.Assert(driver.InternalNameToVolpluginName("tenant1.test"), Equals, "tenant1/test")
	c.Assert(driver.InternalNameToVolpluginName("tenant1.test.two"), Equals, "tenant1/test.two")
}
