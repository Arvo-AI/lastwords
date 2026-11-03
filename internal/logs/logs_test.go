package logs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseCRIJoinsPartials(t *testing.T) {
	in := "2026-01-02T03:04:05.123456789Z stdout P hello \n2026-01-02T03:04:05.2Z stdout F world\n2026-01-02T03:04:06Z stderr F panic: boom\nnot cri\n"
	got := ParseCRI([]byte(in))
	want := []string{"2026-01-02T03:04:05.200Z stdout hello world", "2026-01-02T03:04:06.000Z stderr panic: boom", "not cri"}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: %q want %q", i, got[i], want[i])
		}
	}
}

func TestFindInstanceLogPicksDeadInstance(t *testing.T) {
	d := t.TempDir()
	dead := time.Now().Add(-time.Minute)
	for n, mod := range map[string]time.Time{"0.log": dead.Add(-time.Hour), "1.log": dead, "2.log": time.Now()} {
		p := filepath.Join(d, n)
		os.WriteFile(p, []byte("x"), 0o644)
		os.Chtimes(p, mod, mod)
	}
	got, err := FindInstanceLog(d, -1, dead)
	if err != nil || filepath.Base(got) != "1.log" {
		t.Fatalf("got %s %v", got, err)
	}
	// A fast restart can leave the dead instance's file newer than finishedAt;
	// the attempt number still finds it.
	now := time.Now()
	os.Chtimes(filepath.Join(d, "1.log"), now, now)
	got, err = FindInstanceLog(d, 1, dead)
	if err != nil || filepath.Base(got) != "1.log" {
		t.Fatalf("with attempt: got %s %v", got, err)
	}
}

func TestTailAndInteresting(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "0.log")
	content := ""
	for i := 0; i < 50; i++ {
		content += "2026-01-02T03:04:05Z stdout F line\n"
	}
	content += "2026-01-02T03:04:06Z stderr F panic: runtime error: index out of range\n"
	os.WriteFile(p, []byte(content), 0o644)
	out, err := Tail(p, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(Interesting(out, 5)); n != 1 {
		t.Fatalf("interesting = %d\n%s", n, out)
	}
}

func TestCrashSiteGo(t *testing.T) {
	text := `2026-01-01T00:00:00.000Z stderr panic: assignment to entry in nil map
2026-01-01T00:00:00.000Z stderr 
2026-01-01T00:00:00.000Z stderr goroutine 1 [running]:
2026-01-01T00:00:00.000Z stderr main.registerDiscount(...)
2026-01-01T00:00:00.000Z stderr 	/src/main.go:20
2026-01-01T00:00:00.000Z stderr main.main()
2026-01-01T00:00:00.000Z stderr 	/src/main.go:33 +0x2c0`
	got := CrashSite(text)
	if got != "main.registerDiscount at /src/main.go:20 (assignment to entry in nil map)" {
		t.Fatalf("got %q", got)
	}
}

func TestCrashSiteJava(t *testing.T) {
	text := `Exception in thread "main" java.lang.NullPointerException: Cannot invoke "String.length()"
	at java.base/java.util.Objects.requireNonNull(Objects.java:233)
	at com.example.Billing.total(Billing.java:42)
	at com.example.Main.main(Main.java:10)`
	if got := CrashSite(text); got != `com.example.Billing.total(Billing.java:42) (Exception in thread "main" java.lang.NullPointerException: Cannot invoke "String.length()")` {
		t.Fatalf("got %q", got)
	}
}

func TestCrashSitePython(t *testing.T) {
	text := `Traceback (most recent call last):
  File "/app/worker.py", line 50, in <module>
    main()
  File "/app/worker.py", line 12, in handle
    total = order["amount"]
KeyError: 'amount'`
	if got := CrashSite(text); got != `File "/app/worker.py", line 12, in handle (KeyError: 'amount')` {
		t.Fatalf("got %q", got)
	}
}
