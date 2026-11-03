package procfs

import "testing"

func TestRedactCmdline(t *testing.T) {
	cases := map[string]string{
		"java -Ddb.password=hunter2 -jar app.jar":     "java -Ddb.password=<redacted> -jar app.jar",
		"server --token abc123 --port 80":             "server --token <redacted> --port 80",
		"node index.js --api-key=sk_live_x":           "node index.js --api-key=<redacted>",
		"python app.py --workers 4":                   "python app.py --workers 4",
		"worker --db postgres://app:s3cr3t@db:5432/x": "worker --db postgres://app:<redacted>@db:5432/x",
		"/usr/bin/mysqldump -uroot -phunter2 shop":    "/usr/bin/mysqldump -uroot -p<redacted> shop",
		"nginx -p /etc/nginx":                         "nginx -p /etc/nginx",
	}
	for in, want := range cases {
		if got := RedactCmdline(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestBytes(t *testing.T) {
	if Bytes(3*1024*1024*1024/2) != "1.50 GiB" || Bytes(5*1024*1024) != "5.0 MiB" {
		t.Fatal(Bytes(3*1024*1024*1024/2), Bytes(5*1024*1024))
	}
}
