package main

import (
  "encoding/json"
  "fmt"
  "os"
)
func load(path string, out any) { b, err := os.ReadFile(path); if err != nil { panic(err) }; if err := json.Unmarshal(b, out); err != nil { panic(err) } }
func main() {
  var contract map[string]any; var samples []map[string]any; var rules map[string]any
  load("/workspace/contracts/request.schema.json", &contract); load("/workspace/fixtures/sample-requests.json", &samples); load("/workspace/fixtures/rules.json", &rules)
  if contract["type"] != "object" || len(samples) == 0 || rules["version"] == nil { panic("基础输入无效") }
  fmt.Println("基础输入校验通过")
}
