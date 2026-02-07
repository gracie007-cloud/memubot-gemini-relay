package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// --- 全局变量与标志 ---
var (
	debugMode bool
	proxyURL  string
	apiKey    string = "AIzaSyD81zQQoHvwSVurzOOaWJtGI5ZiARySgwc" // 默认 Key
)

// --- 结构体定义 ---
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type GenericMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type GenericRequest struct {
	Model    string           `json:"model"`
	System   string           `json:"system,omitempty"`
	Messages []GenericMessage `json:"messages"`
}

type GooglePart struct {
	Text             string              `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall `json:"functionCall,omitempty"`
	ThoughtSignature string              `json:"thoughtSignature,omitempty"`
}

type geminiFunctionCall struct {
	Name    string         `json:"name"`
	Args    map[string]any `json:"args"`
	ID      string         `json:"id,omitempty"`
	Thought bool           `json:"thought,omitempty"`
}

type GoogleContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GooglePart `json:"parts"`
}

type GoogleRequest struct {
	Contents []GoogleContent `json:"contents"`
}

type GoogleResponse struct {
	Candidates []struct {
		Content struct {
			Parts []GooglePart `json:"parts"`
		} `json:"content"`
		FinishReason  string `json:"finishReason"`
		FinishMessage string `json:"finishMessage"`
	} `json:"candidates"`
}

func extractText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var combined []string
		for _, b := range blocks {
			if b.Type == "text" {
				combined = append(combined, b.Text)
			}
		}
		return strings.Join(combined, "\n")
	}
	return string(raw)
}

func main() {
	// 解析命令行参数
	flag.BoolVar(&debugMode, "debug", false, "是否开启调试模式")
	flag.StringVar(&proxyURL, "proxy", "", "代理服务器地址 (如 http://127.0.0.1:7890)")
	flag.Parse()

	// 打印启动横幅
	fmt.Println("用于 memU bot 的 Gemini API 中继工具")
	fmt.Println("memU bot 设置如下：")
	fmt.Println("----------------------------------")
	fmt.Println(" LLM 提供商：Custom Provider")
	fmt.Println(" API 地址：http://127.0.0.1:6300/")
	fmt.Println(" API 密钥：【Gemini api key】")
	fmt.Println(" 模型名称：gemini-3-flash-preview")
	fmt.Println("----------------------------------")
	if proxyURL != "" {
		fmt.Printf("已启用代理: %s\n", proxyURL)
	} else {
		fmt.Println("使用 --proxy 让请求通过代理转发")
		fmt.Println("如 --proxy http://127.0.0.1:7890")
	}
	fmt.Println("当前正在中继Gemini api")

	http.HandleFunc("/v1/", handleProxy)
	log.Fatal(http.ListenAndServe(":6300", nil))
}

func handleProxy(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	reqKey := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if reqKey == "" {
		reqKey = r.Header.Get("x-api-key")
	}
	if reqKey == "" {
		reqKey = apiKey
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	var genReq GenericRequest
	if err := json.Unmarshal(bodyBytes, &genReq); err != nil {
		fmt.Printf("[ERR] JSON 解析失败: %v\n", err)
		http.Error(w, "Invalid JSON", 400)
		return
	}

	if debugMode {
		log.Printf("\n[DEBUG] 收到请求: %s %s | 模型: %s", r.Method, path, genReq.Model)
	}

	var gReq GoogleRequest
	if genReq.System != "" {
		gReq.Contents = append(gReq.Contents, GoogleContent{
			Role:  "user",
			Parts: []GooglePart{{Text: "System Instruction: " + genReq.System}},
		})
	}
	for _, m := range genReq.Messages {
		role := "user"
		if m.Role == "assistant" || m.Role == "model" {
			role = "model"
		}
		text := extractText(m.Content)
		gReq.Contents = append(gReq.Contents, GoogleContent{
			Role:  role,
			Parts: []GooglePart{{Text: text}},
		})
	}

	transport := &http.Transport{}
	if proxyURL != "" {
		pURL, _ := url.Parse(proxyURL)
		transport.Proxy = http.ProxyURL(pURL)
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   60 * time.Second,
	}

	googleURL := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", genReq.Model, reqKey)
	payload, _ := json.Marshal(gReq)

	if debugMode {
		log.Printf("[DEBUG] 转发至 Google: %s", genReq.Model)
		log.Printf("[DEBUG] Payload: %s", string(payload))
	}

	gReqObj, _ := http.NewRequest("POST", googleURL, bytes.NewBuffer(payload))
	gReqObj.Header.Set("Content-Type", "application/json")

	startTime := time.Now()
	resp, err := client.Do(gReqObj)
	if err != nil {
		fmt.Printf("[ERR] 网络连接失败: %v\n", err)
		http.Error(w, err.Error(), 502)
		return
	}
	defer resp.Body.Close()

	gBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Printf("[ERR] Google 报错 (状态码 %d): %s\n", resp.StatusCode, string(gBody))
		w.WriteHeader(resp.StatusCode)
		w.Write(gBody)
		return
	}

	var gResp GoogleResponse
	json.Unmarshal(gBody, &gResp)

	if len(gResp.Candidates) > 0 {
		candidate := gResp.Candidates[0]
		content := ""
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				content += part.Text
			}
		}

		// 如果内容为空且发生了 MALFORMED_FUNCTION_CALL，从 FinishMessage 中提取
		if content == "" && candidate.FinishReason == "MALFORMED_FUNCTION_CALL" && candidate.FinishMessage != "" {
			content = candidate.FinishMessage
			// 尝试移除 "Malformed function call: " 前缀
			content = strings.TrimPrefix(content, "Malformed function call: ")
			// 尝试移除可能存在的 hallucinated tool call (例如 call:xxx{...} 或 call:xxx(...))
			if idx := strings.Index(content, "})"); idx != -1 {
				content = content[idx+2:]
			} else if idx := strings.Index(content, "}"); idx != -1 {
				content = content[idx+1:]
			} else if idx := strings.Index(content, ")"); idx != -1 {
				content = content[idx+1:]
			}
			content = strings.TrimSpace(content)
		}

		if content != "" {
			var res interface{}
			if strings.Contains(path, "/messages") {
				res = map[string]interface{}{
					"id":    fmt.Sprintf("ant-%d", time.Now().Unix()),
					"type":  "message",
					"role":  "assistant",
					"model": genReq.Model,
					"content": []map[string]interface{}{
						{"type": "text", "text": content},
					},
					"stop_reason": "end_turn",
				}
			} else {
				res = map[string]interface{}{
					"id":      fmt.Sprintf("chatcmpl-%d", time.Now().Unix()),
					"object":  "chat.completion",
					"created": time.Now().Unix(),
					"model":   genReq.Model,
					"choices": []map[string]interface{}{
						{
							"index": 0,
							"message": map[string]string{
								"role":    "assistant",
								"content": content,
							},
							"finish_reason": "stop",
						},
					},
				}
			}

			if debugMode {
				log.Printf("[DEBUG] 成功响应 | 耗时: %v", time.Since(startTime))
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(res)
		} else {
			fmt.Printf("[ERR] Gemini 未返回有效内容 (可能是被安全策略拦截)。原始响应: %s\n", string(gBody))
			http.Error(w, "Gemini returned no content (possibly blocked by safety filters)", 500)
		}
	} else {
		fmt.Printf("[ERR] Gemini 未返回 Candidate。原始响应: %s\n", string(gBody))
		http.Error(w, "Gemini returned no candidates", 500)
	}
}
