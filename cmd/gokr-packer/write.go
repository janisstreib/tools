package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gokrazy/internal/fat"
	"github.com/gokrazy/internal/mbr"
	"github.com/gokrazy/internal/squashfs"
)

var (
	serialConsole = flag.String("serial_console",
		"ttyAMA0,115200",
		`"ttyAMA0,115200" enables UART0 as a serial console, "disabled" allows applications to use UART0 instead`)

	kernelPackage = flag.String("kernel_package",
		"github.com/gokrazy/kernel",
		"Go package to copy vmlinuz and *.dtb from for constructing the firmware file system")

	firmwarePackage = flag.String("firmware_package",
		"github.com/gokrazy/firmware",
		"Go package to copy *.{bin,dat,elf} from for constructing the firmware file system")
)

func copyFile(fw *fat.Writer, dest, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	w, err := fw.File(dest, st.ModTime())
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, f); err != nil {
		return err
	}
	return f.Close()
}

func copyFileSquash(d *squashfs.Directory, dest, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	w, err := d.File(filepath.Base(dest), st.ModTime(), st.Mode()&os.ModePerm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, f); err != nil {
		return err
	}
	return w.Close()
}

func writeCmdline(fw *fat.Writer, src string, partuuid uint32, usePartuuid bool) error {
	b, err := ioutil.ReadFile(src)
	if err != nil {
		return err
	}
	var cmdline string
	if *serialConsole != "disabled" {
		if *serialConsole == "UART0" {
			// For backwards compatibility, treat the special value UART0 as
			// ttyAMA0,115200:
			cmdline = "console=ttyAMA0,115200 " + string(b)
		} else {
			cmdline = "console=" + *serialConsole + " " + string(b)
		}
	} else {
		cmdline = string(b)
	}

	// TODO: change {gokrazy,rtr7}/kernel/cmdline.txt to contain a dummy PARTUUID=
	if usePartuuid {
		cmdline = strings.ReplaceAll(cmdline,
			"root=/dev/mmcblk0p2",
			fmt.Sprintf("root=PARTUUID=%08x-02", partuuid))
		cmdline = strings.ReplaceAll(cmdline,
			"root=/dev/sda2",
			fmt.Sprintf("root=PARTUUID=%08x-02", partuuid))
	} else {
		log.Printf("(not using PARTUUID= in cmdline.txt yet)")
	}

	w, err := fw.File("/cmdline.txt", time.Now())
	if err != nil {
		return err
	}
	_, err = w.Write([]byte(cmdline))
	return err
}

func writeConfig(fw *fat.Writer, src string) error {
	b, err := ioutil.ReadFile(src)
	if err != nil {
		return err
	}
	config := string(b)
	if *serialConsole != "disabled" {
		config = strings.ReplaceAll(config, "enable_uart=0", "enable_uart=1")
	}
	w, err := fw.File("/config.txt", time.Now())
	if err != nil {
		return err
	}
	_, err = w.Write([]byte(config))
	return err
}

var (
	firmwareGlobs = []string{
		"*.bin",
		"*.dat",
		"*.elf",
		"*.upd",
		"*.sig",
	}
	kernelGlobs = []string{
		"vmlinuz",
		"*.dtb",
	}
)

func writeBoot(f io.Writer, mbrfilename string, partuuid uint32, usePartuuid bool) error {
	log.Printf("writing boot file system")
	globs := make([]string, 0, len(firmwareGlobs)+len(kernelGlobs))
	firmwareDir, err := packageDir(*firmwarePackage)
	if err != nil {
		return err
	}
	for _, glob := range firmwareGlobs {
		globs = append(globs, filepath.Join(firmwareDir, glob))
	}
	kernelDir, err := packageDir(*kernelPackage)
	if err != nil {
		return err
	}
	for _, glob := range kernelGlobs {
		globs = append(globs, filepath.Join(kernelDir, glob))
	}

	bufw := bufio.NewWriter(f)
	fw, err := fat.NewWriter(bufw)
	if err != nil {
		return err
	}
	for _, pattern := range globs {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return err
		}
		for _, m := range matches {
			if err := copyFile(fw, "/"+filepath.Base(m), m); err != nil {
				return err
			}
		}
	}

	if err := writeCmdline(fw, filepath.Join(kernelDir, "cmdline.txt"), partuuid, usePartuuid); err != nil {
		return err
	}

	if err := writeConfig(fw, filepath.Join(kernelDir, "config.txt")); err != nil {
		return err
	}

	if err := fw.Flush(); err != nil {
		return err
	}
	if err := bufw.Flush(); err != nil {
		return err
	}
	if mbrfilename != "" {
		if _, ok := f.(io.ReadSeeker); !ok {
			return fmt.Errorf("BUG: f does not implement io.ReadSeeker")
		}
		fmbr, err := os.OpenFile(mbrfilename, os.O_RDWR|os.O_CREATE, 0600)
		if err != nil {
			return err
		}
		defer fmbr.Close()
		if err := writeMBR(f.(io.ReadSeeker), fmbr, partuuid); err != nil {
			return err
		}
		if err := fmbr.Close(); err != nil {
			return err
		}
	}
	return nil
}

type fileInfo struct {
	filename string

	fromHost    string
	fromLiteral string
	symlinkDest string

	dirents []*fileInfo
}

func (fi *fileInfo) mustFindDirent(path string) *fileInfo {
	for _, ent := range fi.dirents {
		// TODO: split path into components and compare piecemeal
		if ent.filename == path {
			return ent
		}
	}
	log.Panicf("mustFindDirent(%q) did not find directory entry", path)
	return nil
}

func findBins() (*fileInfo, error) {
	result := fileInfo{filename: ""}

	gokrazyMainPkgs, err := mainPackages(gokrazyPkgs)
	if err != nil {
		return nil, err
	}
	gokrazy := fileInfo{filename: "gokrazy"}
	for _, target := range gokrazyMainPkgs {
		gokrazy.dirents = append(gokrazy.dirents, &fileInfo{
			filename: filepath.Base(target),
			fromHost: target,
		})
	}

	if *initPkg != "" {
		initMainPkgs, err := mainPackages([]string{*initPkg})
		if err != nil {
			return nil, err
		}
		for _, target := range initMainPkgs {
			if got, want := filepath.Base(target), "init"; got != want {
				log.Printf("Error: -init_pkg=%q produced unexpected binary name: got %q, want %q", *initPkg, got, want)
				continue
			}
			gokrazy.dirents = append(gokrazy.dirents, &fileInfo{
				filename: "init",
				fromHost: target,
			})
		}
	}
	result.dirents = append(result.dirents, &gokrazy)

	mainPkgs, err := mainPackages(flag.Args())
	if err != nil {
		return nil, err
	}
	user := fileInfo{filename: "user"}
	for _, target := range mainPkgs {
		user.dirents = append(user.dirents, &fileInfo{
			filename: filepath.Base(target),
			fromHost: target,
		})
	}
	result.dirents = append(result.dirents, &user)
	return &result, nil
}

func writeFileInfo(dir *squashfs.Directory, fi *fileInfo) error {
	if fi.fromHost != "" { // copy a regular file
		return copyFileSquash(dir, fi.filename, fi.fromHost)
	}
	if fi.fromLiteral != "" { // write a regular file
		w, err := dir.File(fi.filename, time.Now(), 0444)
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(fi.fromLiteral)); err != nil {
			return err
		}
		return w.Close()
	}

	if fi.symlinkDest != "" { // create a symlink
		return dir.Symlink(fi.symlinkDest, fi.filename, time.Now(), 0444)
	}
	// subdir
	var d *squashfs.Directory
	if fi.filename == "" { // root
		d = dir
	} else {
		d = dir.Directory(fi.filename, time.Now())
	}
	sort.Slice(fi.dirents, func(i, j int) bool {
		return fi.dirents[i].filename < fi.dirents[j].filename
	})
	for _, ent := range fi.dirents {
		if err := writeFileInfo(d, ent); err != nil {
			return err
		}
	}
	return d.Flush()
}

func writeRoot(f io.WriteSeeker, root *fileInfo) error {
	log.Printf("writing root file system")
	fw, err := squashfs.NewWriter(f, time.Now())
	if err != nil {
		return err
	}

	if err := writeFileInfo(fw.Root, root); err != nil {
		return err
	}

	return fw.Flush()
}

func writeMBR(f io.ReadSeeker, fw io.WriteSeeker, partuuid uint32) error {
	rd, err := fat.NewReader(f)
	if err != nil {
		return err
	}
	vmlinuzOffset, _, err := rd.Extents("/vmlinuz")
	if err != nil {
		return err
	}
	cmdlineOffset, _, err := rd.Extents("/cmdline.txt")
	if err != nil {
		return err
	}

	if _, err := fw.Seek(0, io.SeekStart); err != nil {
		return err
	}
	vmlinuzLba := uint32((vmlinuzOffset / 512) + 8192)
	cmdlineTxtLba := uint32((cmdlineOffset / 512) + 8192)

	log.Printf("writing MBR (LBAs: vmlinuz=%d, cmdline.txt=%d, PARTUUID=%08x)", vmlinuzLba, cmdlineTxtLba, partuuid)
	mbr := mbr.Configure(vmlinuzLba, cmdlineTxtLba, partuuid)
	if _, err := fw.Write(mbr[:]); err != nil {
		return err
	}

	return nil
}
