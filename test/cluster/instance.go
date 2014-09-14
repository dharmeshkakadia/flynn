package cluster

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/flynn/flynn/Godeps/_workspace/src/code.google.com/p/go.crypto/ssh"
	"github.com/flynn/flynn/pkg/attempt"
	"github.com/flynn/flynn/pkg/random"
)

func NewVMManager(bridge *Bridge) *VMManager {
	return &VMManager{taps: &TapManager{bridge}}
}

type VMManager struct {
	taps *TapManager
}

type VMConfig struct {
	Kernel string
	User   int
	Group  int
	Memory string
	Cores  int
	Drives map[string]*VMDrive
	Args   []string
	Out    io.Writer

	netFS string
}

type VMDrive struct {
	FS   string
	COW  bool
	Temp bool
}

func (v *VMManager) NewInstance(c *VMConfig) (Instance, error) {
	inst := &vm{VMConfig: c, id: random.String(8)}
	if c.Kernel == "" {
		c.Kernel = "vmlinuz"
	}
	if c.Out == nil {
		var err error
		c.Out, err = os.Create("flynn-" + inst.ID() + ".log")
		if err != nil {
			return nil, err
		}
	}
	var err error
	inst.tap, err = v.taps.NewTap(c.User, c.Group)
	return inst, err
}

type Instance interface {
	ID() string
	DialSSH() (*ssh.Client, error)
	Start() error
	Wait(time.Duration) error
	Shutdown() error
	Kill() error
	IP() string
	Run(string, *Streams) error
	Drive(string) *VMDrive
}

type vm struct {
	id string
	*VMConfig
	tap *Tap
	cmd *exec.Cmd

	tempFiles []string
}

func (v *vm) writeInterfaceConfig() error {
	dir, err := ioutil.TempDir("", "netfs-")
	if err != nil {
		return err
	}
	v.tempFiles = append(v.tempFiles, dir)
	v.netFS = dir

	if err := os.Chmod(dir, 0755); err != nil {
		os.RemoveAll(dir)
		return err
	}

	f, err := os.Create(filepath.Join(dir, "eth0"))
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	defer f.Close()

	return v.tap.WriteInterfaceConfig(f)
}

func (v *vm) cleanup() {
	for _, f := range v.tempFiles {
		if err := os.RemoveAll(f); err != nil {
			fmt.Printf("could not remove temp file %s: %s\n", f, err)
		}
	}
	if err := v.tap.Close(); err != nil {
		fmt.Printf("could not close tap device %s: %s\n", v.tap.Name, err)
	}
	v.tempFiles = nil
}

func (v *vm) Start() error {
	v.writeInterfaceConfig()

	macRand := random.Bytes(3)
	macaddr := fmt.Sprintf("52:54:00:%02x:%02x:%02x", macRand[0], macRand[1], macRand[2])

	v.Args = append(v.Args,
		"-enable-kvm",
		"-kernel", v.Kernel,
		"-append", `"root=/dev/sda"`,
		"-net", "nic,macaddr="+macaddr,
		"-net", "tap,ifname="+v.tap.Name+",script=no,downscript=no",
		"-virtfs", "fsdriver=local,path="+v.netFS+",security_model=passthrough,readonly,mount_tag=netfs",
		"-nographic",
	)
	if v.Memory != "" {
		v.Args = append(v.Args, "-m", v.Memory)
	}
	if v.Cores > 0 {
		v.Args = append(v.Args, "-smp", strconv.Itoa(v.Cores))
	}
	var err error
	for i, d := range v.Drives {
		if d.COW {
			fs, err := v.createCOW(d.FS, d.Temp)
			if err != nil {
				v.cleanup()
				return err
			}
			d.FS = fs
		}
		v.Args = append(v.Args, fmt.Sprintf("-%s", i), d.FS)
	}

	v.cmd = exec.Command("sudo", append([]string{"-u", fmt.Sprintf("#%d", v.User), "-g", fmt.Sprintf("#%d", v.Group), "-H", "/usr/bin/qemu-system-x86_64"}, v.Args...)...)
	v.cmd.Stdout = v.Out
	v.cmd.Stderr = v.Out
	if err = v.cmd.Start(); err != nil {
		v.cleanup()
	}
	return err
}

func (v *vm) createCOW(image string, temp bool) (string, error) {
	name := strings.TrimSuffix(filepath.Base(image), filepath.Ext(image))
	dir, err := ioutil.TempDir("", name+"-")
	if err != nil {
		return "", err
	}
	if temp {
		v.tempFiles = append(v.tempFiles, dir)
	}
	if err := os.Chown(dir, v.User, v.Group); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "rootfs.img")
	cmd := exec.Command("qemu-img", "create", "-f", "qcow2", "-b", image, path)
	if err = cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to create COW filesystem: %s", err.Error())
	}
	if err := os.Chown(path, v.User, v.Group); err != nil {
		return "", err
	}
	return path, nil
}

func (v *vm) Wait(timeout time.Duration) error {
	done := make(chan error)
	go func() {
		done <- v.cmd.Wait()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return errors.New("timeout")
	}
}

func (v *vm) Shutdown() error {
	if err := v.Run("sudo poweroff", nil); err != nil {
		return v.Kill()
	}
	if err := v.Wait(5 * time.Second); err != nil {
		return v.Kill()
	}
	v.cleanup()
	return nil
}

func (v *vm) Kill() error {
	defer v.cleanup()
	if err := v.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	if err := v.Wait(5 * time.Second); err != nil {
		return v.cmd.Process.Kill()
	}
	return nil
}

func (v *vm) DialSSH() (*ssh.Client, error) {
	return ssh.Dial("tcp", v.IP()+":22", &ssh.ClientConfig{
		User: "ubuntu",
		Auth: []ssh.AuthMethod{ssh.Password("ubuntu")},
	})
}

func (v *vm) ID() string {
	return v.id
}

func (v *vm) IP() string {
	return v.tap.RemoteIP.String()
}

var sshAttempts = attempt.Strategy{
	Min:   5,
	Total: 5 * time.Minute,
	Delay: time.Second,
}

func (v *vm) Run(command string, s *Streams) error {
	if s == nil {
		s = &Streams{}
	}
	var sc *ssh.Client
	err := sshAttempts.Run(func() (err error) {
		if s.Stderr != nil {
			fmt.Fprintf(s.Stderr, "Attempting to ssh to %s:22...\n", v.IP())
		}
		sc, err = v.DialSSH()
		return
	})
	if err != nil {
		return err
	}
	defer sc.Close()
	sess, err := sc.NewSession()
	sess.Stdin = s.Stdin
	sess.Stdout = s.Stdout
	sess.Stderr = s.Stderr
	if err := sess.Run(command); err != nil {
		return fmt.Errorf("failed to run command on %s: %s", v.IP(), err)
	}
	return nil
}

func (v *vm) Drive(name string) *VMDrive {
	return v.Drives[name]
}
