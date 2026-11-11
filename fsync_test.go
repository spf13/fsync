package fsync

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/afero"
)

func TestSync(t *testing.T) {
	// create test directory and chdir to it
	dir := t.TempDir()
	t.Chdir(dir)

	// create test files and directories
	check(os.MkdirAll("src/a", 0o755))
	check(os.WriteFile("src/a/b", []byte("file b"), 0o644))
	check(os.WriteFile("src/c", []byte("file c"), 0o644))
	// set times in the past to make sure times are synced, not accidentally
	// the same
	tt := time.Now().Add(-1 * time.Hour)
	check(os.Chtimes("src/a/b", tt, tt))
	check(os.Chtimes("src/a", tt, tt))
	check(os.Chtimes("src/c", tt, tt))
	check(os.Chtimes("src", tt, tt))

	// create Syncer
	s := NewSyncer()

	// sync
	check(s.SyncTo("dst", "src/a", "src/c"))

	// check results
	testDirContents("dst", 2, t)
	testDirContents("dst/a", 1, t)
	testFile("dst/a/b", []byte("file b"), t)
	testFile("dst/c", []byte("file c"), t)
	testPerms("dst/a", getPerms("src/a"), t)
	testPerms("dst/a/b", getPerms("src/a/b"), t)
	testPerms("dst/c", getPerms("src/c"), t)
	testModTime("dst/a", getModTime("src/a"), t)
	testModTime("dst/a/b", getModTime("src/a/b"), t)
	testModTime("dst/c", getModTime("src/c"), t)

	// sync the parent directory too
	check(s.Sync("dst", "src"))

	// check the results
	testPerms("dst", getPerms("src"), t)
	testModTime("dst", getModTime("src"), t)

	// modify src
	check(os.WriteFile("src/a/b", []byte("file b changed"), 0o644))
	check(os.Chmod("src/a", 0o775))

	// sync
	check(s.Sync("dst", "src"))

	// check results
	testFile("dst/a/b", []byte("file b changed"), t)
	testPerms("dst/a", getPerms("src/a"), t)
	testModTime("dst", getModTime("src"), t)
	testModTime("dst/a", getModTime("src/a"), t)
	testModTime("dst/a/b", getModTime("src/a/b"), t)
	testModTime("dst/c", getModTime("src/c"), t)

	// remove c
	check(os.Remove("src/c"))

	// sync
	check(s.Sync("dst", "src"))

	// check results; c should still exist
	testDirContents("dst", 2, t)
	testExistence("dst/c", true, t)

	// sync
	s.Delete = true
	check(s.Sync("dst", "src"))

	// check results; c should no longer exist
	testDirContents("dst", 1, t)
	testExistence("dst/c", false, t)

	s.Delete = false
	if err := s.Sync("dst", "src/a/b"); err == nil {
		t.Errorf("expecting ErrFileOverDir, got nothing.\n")
	} else if err != nil && err != ErrFileOverDir {
		panic(err)
	}
}

// A directory in MemMapFs reports a size of 42, as does a directory on
// Windows (0) when the source file is empty. The stale directory FileInfo
// must not be used to compare the removed dst with the src file.
func TestFileOverDirSameSize(t *testing.T) {
	src := afero.NewMemMapFs()
	dst := afero.NewMemMapFs()
	content := bytes.Repeat([]byte("x"), 42)
	check(afero.WriteFile(src, "a", content, 0o644))
	check(dst.MkdirAll("a", 0o755))

	s := NewSyncer()
	s.SrcFs = src
	s.DestFs = dst
	check(s.Sync("a", "a"))

	b, err := afero.ReadFile(dst, "a")
	check(err)
	if !bytes.Equal(b, content) {
		t.Fatalf("got %q", b)
	}
}

func TestEquality(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// a file should obviously be equal to itself
	check(os.MkdirAll("src/", 0o755))
	check(os.WriteFile("src/a", []byte("file a"), 0o644))
	testEqual("src/a", "src/a", t)

	// even if's referred to by different ways
	testEqual("src/a", "./src/a", t)

	t.Run("symbolic links", func(t *testing.T) {
		check(os.MkdirAll("dst/symlink", 0o755))
		// (need a ../.. in the symlink because that's the name of the file from the new
		// file's perspective; in a hard link, it's the name from *our* perspective)
		if err := os.Symlink("../../src/a", "dst/symlink/b"); err != nil {
			t.Skipf("No symbolic links allowed? %v", err)
		}
		testEqual("src/a", "dst/symlink/b", t)
	})

	t.Run("hard links", func(t *testing.T) {
		check(os.MkdirAll("dst/hardlink", 0o755))
		if err := os.Link("src/a", "dst/hardlink/b"); err != nil {
			t.Skipf("No hard links allowed? %v", err)
		}
		testEqual("src/a", "dst/hardlink/b", t)
	})
}

func TestEqualQuickCheck(t *testing.T) {
	newFs := func() (afero.Fs, afero.Fs) {
		src := afero.NewMemMapFs()
		dst := afero.NewMemMapFs()
		tt := time.Now().Add(-1 * time.Hour)
		check(afero.WriteFile(src, "a", []byte("file a"), 0o644))
		check(src.Chtimes("a", tt, tt))
		// Same size and modification time, different content.
		check(afero.WriteFile(dst, "a", []byte("file A"), 0o644))
		check(dst.Chtimes("a", tt, tt))
		return src, dst
	}

	t.Run("Same size and modtime", func(t *testing.T) {
		src, dst := newFs()
		s := NewSyncer()
		s.SrcFs, s.DestFs = src, dst
		s.checkContents = func(s *Syncer, dst, src string) bool {
			t.Fatalf("Asked to look at file contents of %s vs %s", dst, src)
			return false
		}
		check(s.Sync("a", "a"))
		b, err := afero.ReadFile(dst, "a")
		check(err)
		if string(b) != "file A" {
			t.Fatalf("expected dst to be left untouched, got %q", b)
		}
	})

	t.Run("NoTimes", func(t *testing.T) {
		src, dst := newFs()
		s := NewSyncer()
		s.SrcFs, s.DestFs = src, dst
		s.NoTimes = true
		check(s.Sync("a", "a"))
		b, err := afero.ReadFile(dst, "a")
		check(err)
		if string(b) != "file a" {
			t.Fatalf("expected dst to be updated, got %q", b)
		}
	})
}

func TestCheckContents(t *testing.T) {
	fs := afero.NewMemMapFs()
	s := NewSyncer()
	s.SrcFs, s.DestFs = fs, fs

	for _, size := range []int{0, 1, compareBufSize - 1, compareBufSize, compareBufSize + 1, 3*compareBufSize + 7} {
		content := bytes.Repeat([]byte("x"), size)
		check(afero.WriteFile(fs, "a", content, 0o644))
		check(afero.WriteFile(fs, "b", content, 0o644))
		if !defaultCheckContents(s, "a", "b") {
			t.Errorf("size %d: expected equal", size)
		}
		if size == 0 {
			continue
		}
		changed := bytes.Clone(content)
		changed[size-1] = 'y'
		check(afero.WriteFile(fs, "b", changed, 0o644))
		if defaultCheckContents(s, "a", "b") {
			t.Errorf("size %d: expected different", size)
		}
	}
}

func TestDeleteFileFilter(t *testing.T) {
	// create test directory and chdir to it
	dir := t.TempDir()
	t.Chdir(dir)

	// create test files and directories
	check(os.MkdirAll("src/a", 0o755))
	check(os.WriteFile("src/a/b", []byte("file b"), 0o644))

	check(os.MkdirAll("dst", 0o755))
	check(os.WriteFile("dst/c", []byte("file c"), 0o644))
	check(os.WriteFile("dst/d", []byte("file c"), 0o644))

	// create Syncer
	s := NewSyncer()
	s.Delete = true

	s.DeleteFilter = func(f FileInfo) bool {
		return f.Name() == "d"
	}

	// precondition; dst contains 2 files `c` and `d`
	testDirContents("dst", 2, t)
	testExistence("dst/c", true, t)
	testExistence("dst/d", true, t)

	check(s.Sync("dst", "src"))

	// check results; c should no longer exist
	testDirContents("dst", 2, t)
	testExistence("dst/a/", true, t)
	testExistence("dst/a/b", true, t)
	testExistence("dst/c", false, t)
	testExistence("dst/d", true, t)
}

func TestDeleteFileFilterNotSet(t *testing.T) {
	// create test directory and chdir to it
	dir := t.TempDir()
	t.Chdir(dir)

	// create test files and directories
	check(os.MkdirAll("src/a", 0o755))
	check(os.WriteFile("src/a/b", []byte("file b"), 0o644))

	check(os.MkdirAll("dst", 0o755))
	check(os.WriteFile("dst/c", []byte("file c"), 0o644))
	check(os.WriteFile("dst/d", []byte("file c"), 0o644))

	// create Syncer
	s := NewSyncer()
	s.Delete = true

	// precondition; dst contains 2 files `c` and `d`
	testDirContents("dst", 2, t)
	testExistence("dst/c", true, t)
	testExistence("dst/d", true, t)

	check(s.Sync("dst", "src"))

	// check results; c should no longer exist
	testDirContents("dst", 1, t)
	testExistence("dst/a/", true, t)
	testExistence("dst/a/b", true, t)
	testExistence("dst/c", false, t)
	testExistence("dst/d", false, t)
}

func testFile(name string, b []byte, t *testing.T) {
	testExistence(name, true, t)
	c, err := os.ReadFile(name)
	check(err)
	if !bytes.Equal(b, c) {
		t.Errorf("content of file \"%s\" is:\n%s\nexpected:\n%s\n",
			name, c, b)
	}
}

func testExistence(name string, e bool, t *testing.T) {
	_, err := os.Stat(name)
	if os.IsNotExist(err) {
		if e {
			t.Errorf("file \"%s\" does not exist.\n", name)
		}
	} else if err != nil {
		panic(err)
	} else {
		if !e {
			t.Errorf("file \"%s\" exists.\n", name)
		}
	}
}

func testDirContents(name string, count int, t *testing.T) {
	files, err := os.ReadDir(name)
	check(err)
	if len(files) != count {
		t.Errorf("directory \"%s\" has %d children, shoud have %d.\n",
			name, len(files), count)
	}
}

func testPerms(name string, p os.FileMode, t *testing.T) {
	p2 := getPerms(name)
	if p2 != p {
		t.Errorf("permissions for \"%s\" is %v, should be %v.\n",
			name, p2, p)
	}
}

func testModTime(name string, m time.Time, t *testing.T) {
	m2 := getModTime(name)
	if !m2.Equal(m) {
		t.Errorf("modification time for \"%s\" is %v, should be %v.\n",
			name, m2, m)
	}
}

func testEqual(src, dst string, t *testing.T) {
	s := NewSyncer()
	s.checkContents = func(s *Syncer, dst, src string) bool {
		t.Errorf("Asked to look at file contents of %s vs %s", dst, src)
		return false
	}
	dstat, err := s.DestFs.Stat(dst)
	check(err)
	sstat, err := s.SrcFs.Stat(src)
	check(err)
	if !s.equal(dst, src, dstat, sstat) {
		t.Errorf("Files %s and %s unexpectedly different", dst, src)
	}
}

func getPerms(name string) os.FileMode {
	info, err := os.Stat(name)
	check(err)
	return info.Mode().Perm()
}

func getModTime(name string) time.Time {
	info, err := os.Stat(name)
	check(err)
	return info.ModTime()
}

func BenchmarkSync(b *testing.B) {
	createSomeFilesIn := func(tempDir, dirname string) {
		for i := 0; i < 5; i++ {
			f, err := os.Create(filepath.Join(tempDir, dirname, fmt.Sprintf("test%d.txt", i)))
			if err != nil {
				b.Fatal(err)
			}
			f.Close()
		}
	}
	createSomeFiles := func() string {
		tempDir := b.TempDir()
		depth := 5
		for level := depth; level > 0; level-- {
			dirname := ""
			for i := 0; i < level; i++ {
				dirname = filepath.Join(dirname, fmt.Sprintf("dir%d", i))
				err := os.MkdirAll(filepath.Join(tempDir, dirname), 0o755)
				if err != nil && !os.IsExist(err) {
					b.Fatal(err)
				}
			}
			createSomeFilesIn(tempDir, dirname)
		}

		return tempDir
	}

	b.Run("No changes", func(b *testing.B) {
		dst := b.TempDir()
		src := createSomeFiles()
		s := NewSyncer()
		check(s.SyncTo(dst, filepath.Join(src, "dir0")))
		b.ResetTimer()
		for b.Loop() {
			s := NewSyncer()
			check(s.SyncTo(dst, filepath.Join(src, "dir0")))
		}
	})

	createBigFile := func(dir string, size int) {
		check(os.MkdirAll(dir, 0o755))
		check(os.WriteFile(filepath.Join(dir, "big.bin"), bytes.Repeat([]byte("abcdefgh"), size/8), 0o644))
	}

	b.Run("Big file no changes", func(b *testing.B) {
		src := filepath.Join(b.TempDir(), "src")
		dst := filepath.Join(b.TempDir(), "dst")
		createBigFile(src, 64<<20)
		check(NewSyncer().Sync(dst, src))
		b.ResetTimer()
		for b.Loop() {
			check(NewSyncer().Sync(dst, src))
		}
	})

	b.Run("Big file no changes, modtime differs", func(b *testing.B) {
		src := filepath.Join(b.TempDir(), "src")
		dst := filepath.Join(b.TempDir(), "dst")
		createBigFile(src, 64<<20)
		createBigFile(dst, 64<<20)
		b.ResetTimer()
		for b.Loop() {
			s := NewSyncer()
			s.NoTimes = true
			check(s.Sync(dst, src))
		}
	})

	b.Run("Empty dst", func(b *testing.B) {
		for b.Loop() {
			b.StopTimer()
			dst := b.TempDir()
			src := createSomeFiles()
			s := NewSyncer()
			b.StartTimer()
			check(s.SyncTo(dst, filepath.Join(src, "dir0")))
		}
	})
}
