package integration_tests

// File reading, writing, modification, truncate

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"runtime"
	"sync"
	"testing"
)

var plaintextNames bool

// This is the entry point for the tests
func TestMain(m *testing.M) {
	var defaultonly bool
	flag.BoolVar(&defaultonly, "defaultonly", false, "Only test default configuration (openssl=true, plaintextnames=false)")
	flag.Parse()

	if testing.Verbose() {
		fmt.Println("***** Testing with OpenSSL")
	}
	resetTmpDir() // <- this also create gocryptfs.diriv
	mount(defaultCipherDir, defaultPlainDir, "--zerokey")
	r := m.Run()
	unmount(defaultPlainDir)

	if r != 0 {
		os.Exit(r)
	}

	if defaultonly {
		os.Exit(r)
	}

	if testing.Verbose() {
		fmt.Println("***** Testing with native Go crypto")
	}
	resetTmpDir()
	mount(defaultCipherDir, defaultPlainDir, "--zerokey", "--openssl=false")
	r = m.Run()
	unmount(defaultPlainDir)

	if r != 0 {
		os.Exit(r)
	}

	if testing.Verbose() {
		fmt.Println("***** Testing \"--plaintextnames\"")
	}
	resetTmpDir()
	mount(defaultCipherDir, defaultPlainDir, "--zerokey", "--plaintextnames")
	plaintextNames = true
	r = m.Run()
	unmount(defaultPlainDir)

	if r != 0 {
		os.Exit(r)
	}

	os.Exit(r)
}

// Write "n" zero bytes to filename "fn", read again, compare hash
func testWriteN(t *testing.T, fn string, n int) string {
	file, err := os.Create(defaultPlainDir + fn)
	if err != nil {
		t.Fatal(err)
	}

	d := make([]byte, n)
	written, err := file.Write(d)
	if err != nil || written != len(d) {
		t.Errorf("err=\"%s\", written=%d", err, written)
	}
	err = file.Close()
	if err != nil {
		t.Error(err)
	}

	verifySize(t, defaultPlainDir+fn, n)

	bin := md5.Sum(d)
	hashWant := hex.EncodeToString(bin[:])

	hashActual := md5fn(defaultPlainDir + fn)

	if hashActual != hashWant {
		t.Errorf("Wrong content, hashWant=%s hashActual=%s", hashWant, hashActual)
	}

	return hashActual
}

func TestWrite10(t *testing.T) {
	testWriteN(t, "10", 10)
}

func TestWrite100(t *testing.T) {
	testWriteN(t, "100", 100)
}

func TestWrite1M(t *testing.T) {
	testWriteN(t, "1M", 1024*1024)
}

func TestWrite1Mx100(t *testing.T) {
	hashWant := testWriteN(t, "1Mx100", 1024*1024)
	// Read and check 100 times to catch race conditions
	var i int
	for i = 0; i < 100; i++ {
		hashActual := md5fn(defaultPlainDir + "1M")
		if hashActual != hashWant {
			fmt.Printf("Read corruption in loop # %d\n", i)
			t.FailNow()
		} else {
			//fmt.Print(".")
		}
	}
}

func TestTruncate(t *testing.T) {
	fn := defaultPlainDir + "truncate"
	file, err := os.Create(fn)
	if err != nil {
		t.FailNow()
	}
	// Grow to two blocks
	file.Truncate(7000)
	verifySize(t, fn, 7000)
	if md5fn(fn) != "95d4ec7038e3e4fdbd5f15c34c3f0b34" {
		t.Errorf("wrong content")
	}
	// Shrink - needs RMW
	file.Truncate(6999)
	verifySize(t, fn, 6999)
	if md5fn(fn) != "35fd15873ec6c35380064a41b9b9683b" {
		t.Errorf("wrong content")
	}
	// Shrink to one partial block
	file.Truncate(465)
	verifySize(t, fn, 465)
	if md5fn(fn) != "a1534d6e98a6b21386456a8f66c55260" {
		t.Errorf("wrong content")
	}
	// Grow to exactly one block
	file.Truncate(4096)
	verifySize(t, fn, 4096)
	if md5fn(fn) != "620f0b67a91f7f74151bc5be745b7110" {
		t.Errorf("wrong content")
	}
}

func TestAppend(t *testing.T) {
	fn := defaultPlainDir + "append"
	file, err := os.Create(fn)
	if err != nil {
		t.FailNow()
	}
	data := []byte("testdata123456789") // length 17
	var buf bytes.Buffer
	var hashWant string
	for i := 0; i <= 500; i++ {
		file.Write(data)
		buf.Write(data)
		bin := md5.Sum(buf.Bytes())
		hashWant = hex.EncodeToString(bin[:])
		hashActual := md5fn(fn)
		if hashWant != hashActual {
			t.FailNow()
		}
	}

	// Overwrite with the same data
	// Hash must stay the same
	file.Seek(0, 0)
	for i := 0; i <= 500; i++ {
		file.Write(data)
		hashActual := md5fn(fn)
		if hashWant != hashActual {
			t.FailNow()
		}
	}
}

// Create a file with holes by writing to offset 0 (block #0) and
// offset 4096 (block #1).
func TestFileHoles(t *testing.T) {
	fn := defaultPlainDir + "fileholes"
	file, err := os.Create(fn)
	if err != nil {
		t.Errorf("file create failed")
	}
	foo := []byte("foo")
	file.Write(foo)
	file.WriteAt(foo, 4096)
	_, err = ioutil.ReadFile(fn)
	if err != nil {
		t.Error(err)
	}
}

// sContains - does the slice of strings "haystack" contain "needle"?
func sContains(haystack []string, needle string) bool {
	for _, element := range haystack {
		if element == needle {
			return true
		}
	}
	return false
}

func TestRmwRace(t *testing.T) {

	runtime.GOMAXPROCS(10)

	fn := defaultPlainDir + "rmwrace"
	f1, err := os.Create(fn)
	if err != nil {
		t.Fatalf("file create failed")
	}
	f2, err := os.Create(fn)
	if err != nil {
		t.Fatalf("file create failed")
	}

	oldBlock := bytes.Repeat([]byte("o"), 4096)

	newBlock := bytes.Repeat([]byte("n"), 4096)

	shortBlock := bytes.Repeat([]byte("s"), 16)

	mergedBlock := make([]byte, 4096)
	copy(mergedBlock, newBlock)
	copy(mergedBlock[4080:], shortBlock)

	goodMd5 := make(map[string]int)

	for i := 0; i < 1000; i++ {
		// Reset to [ooooooooo]
		_, err = f1.WriteAt(oldBlock, 0)
		if err != nil {
			t.Fatalf("Write failed")
		}

		var wg sync.WaitGroup
		wg.Add(2)

		// Write to the end of the file, [....ssss]
		go func() {
			f1.WriteAt(shortBlock, 4080)
			wg.Done()
		}()

		// Overwrite to [nnnnnnn]
		go func() {
			f2.WriteAt(newBlock, 0)
			wg.Done()
		}()

		wg.Wait()

		// The file should be either:
		// [nnnnnnnnnn] (md5: 6c1660fdabccd448d1359f27b3db3c99) or
		// [nnnnnnssss] (md5: da885006a6a284530a427c73ce1e5c32)
		// but it must not be
		// [oooooossss]

		buf, _ := ioutil.ReadFile(fn)
		m := md5hex(buf)
		goodMd5[m] = goodMd5[m] + 1

		/*
			if m == "6c1660fdabccd448d1359f27b3db3c99" {
				fmt.Println(hex.Dump(buf))
				t.FailNow()
			}
		*/
	}
	if testing.Verbose() {
		fmt.Println(goodMd5)
	}
}

// With "--plaintextnames", the name "/gocryptfs.conf" is reserved.
// Otherwise there should be no restrictions.
func TestFiltered(t *testing.T) {
	filteredFile := defaultPlainDir + "gocryptfs.conf"
	file, err := os.Create(filteredFile)
	if plaintextNames == true && err == nil {
		t.Errorf("should have failed but didn't")
	} else if plaintextNames == false && err != nil {
		t.Error(err)
	}
	file.Close()

	err = os.Remove(filteredFile)
	if plaintextNames == true && err == nil {
		t.Errorf("should have failed but didn't")
	} else if plaintextNames == false && err != nil {
		t.Error(err)
	}
}

func TestFilenameEncryption(t *testing.T) {
	file, err := os.Create(defaultPlainDir + "TestFilenameEncryption.txt")
	file.Close()
	if err != nil {
		t.Fatal(err)
	}

	_, err = os.Stat(defaultCipherDir + "TestFilenameEncryption.txt")
	if plaintextNames == true && err != nil {
		t.Errorf("plaintextnames not working: %v", err)
	} else if plaintextNames == false && err == nil {
		t.Errorf("file name encryption not working")
	}
}

// Test Mkdir and Rmdir
func TestMkdirRmdir(t *testing.T) {
	testMkdirRmdir(t, defaultPlainDir)
}

// Test Rename
func TestRename(t *testing.T) {
	testRename(t, defaultPlainDir)
}

// Overwrite an empty directory with another directory
func TestDirOverwrite(t *testing.T) {
	dir1 := defaultPlainDir + "DirOverwrite1"
	dir2 := defaultPlainDir + "DirOverwrite2"
	err := os.Mkdir(dir1, 0777)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Mkdir(dir2, 0777)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Rename(dir1, dir2)
	if err != nil {
		t.Fatal(err)
	}
}