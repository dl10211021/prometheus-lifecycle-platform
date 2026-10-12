#!/usr/bin/env bash
# A controlled protocol/process fixture, never a system service.
set -euo pipefail
output="$1"
source_dir=$(mktemp -d)
trap 'rm -rf "$source_dir"' EXIT
cat > "$source_dir/main.go" <<'GO'
package main
import ("flag"; "fmt"; "log"; "net/http")
func main() {
 address:=flag.String("web.listen-address","127.0.0.1:19198","fixture endpoint")
 version:=flag.Bool("version",false,"version")
 flag.Parse()
 if *version {fmt.Println("node_exporter, version 1.8.2 (closure fixture)");return}
 http.HandleFunc("/metrics",func(w http.ResponseWriter,r *http.Request) {
  w.Header().Set("Content-Type","text/plain")
  fmt.Fprintln(w,"node_exporter_build_info{version=\"1.8.2\"} 1")
 })
 log.Fatal(http.ListenAndServe(*address,nil))
}
GO
mkdir -p "$(dirname "$output")"
CGO_ENABLED=0 go build -o "$output" "$source_dir/main.go"
