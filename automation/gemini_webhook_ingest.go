// ============================================================================
// Fratuci Labs — Gemini-to-GitHub Automated Bridge (Zero Technical Barrier)
// Author: Antônio — AI Engineering & Autonomous Agents
// Infra Reviewer: Anderson (@fratuci) — Cloud Run Scale-to-Zero Ready
// ============================================================================
// Descrição:
// Este serviço recebe resumos estruturados gerados pela Adriana (Estratégia &
// Farmácia) e pelo André (Jurídico & LegalOps) a partir de suas conversas no
// Google Gemini App / Spark e os converte automaticamente em Commits ou
// Pull Requests auditáveis dentro da organização `fratuci-labs` no GitHub.
// ============================================================================

package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// IngestPayload representa o pacote enviado pelo conector do Gemini da Adriana ou do André.
type IngestPayload struct {
	AuthorRole   string `json:"author_role"`   // Ex: "Adriana — Empreendedorismo & Farmácia" ou "André — Jurídico & Compliance"
	TargetRepo   string `json:"target_repo"`   // Ex: "strategy-and-healthtech" ou "legal-governance-as-code"
	TargetPath   string `json:"target_path"`   // Ex: "historico-gemini/2026-10-estudo-viabilidade-healthtech.md"
	CommitTitle  string `json:"commit_title"`  // Ex: "docs(healthtech): adiciona estudo de viabilidade via Gemini"
	MarkdownBody string `json:"markdown_body"` // Conteúdo completo com Frontmatter YAML
	Branch       string `json:"branch"`        // Padrão: "main" ou branch de feature para PR
}

const allowedGitHubOrg = "fratuci-labs"

var allowedRepositories = map[string]bool{
	"strategy-and-healthtech":  true,
	"legal-governance-as-code": true,
	"ai-agents-core":           true,
	"gcp-data-foundation":      true,
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("/v1/gemini-ingest", handleGeminiIngest)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("[fratuci-labs/gemini-bridge] Servidor iniciado na porta %s (Org restrita: %s)", port, allowedGitHubOrg)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Erro fatal no servidor HTTP: %v", err)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok","org":"fratuci-labs","service":"gemini-webhook-ingest"}`))
}

func handleGeminiIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Método não permitido", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
	if err != nil {
		http.Error(w, "Falha ao ler payload", http.StatusBadRequest)
		return
	}

	// 1. Validação de Assinatura Criptográfica HMAC-SHA256
	webhookSecret := os.Getenv("GEMINI_WEBHOOK_HMAC_SECRET")
	if webhookSecret != "" {
		sigHeader := r.Header.Get("X-Fratuci-Signature-256")
		if !verifyHMAC(bodyBytes, sigHeader, webhookSecret) {
			http.Error(w, "Assinatura HMAC inválida", http.StatusUnauthorized)
			return
		}
	}

	var payload IngestPayload
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		http.Error(w, "JSON malformado: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 2. Guardrail Estrito de Perímetro: Apenas repositórios autorizados da fratuci-labs
	if !allowedRepositories[payload.TargetRepo] {
		http.Error(w, fmt.Sprintf("Repositório '%s' fora da lista permitida na organização %s", payload.TargetRepo, allowedGitHubOrg), http.StatusForbidden)
		return
	}
	if strings.Contains(payload.TargetPath, "..") || strings.HasPrefix(payload.TargetPath, "/") {
		http.Error(w, "Caminho de arquivo inválido", http.StatusBadRequest)
		return
	}
	if payload.Branch == "" {
		payload.Branch = "main"
	}

	// 3. Calcula o Hash SHA-256 do documento para a trilha de auditoria do ClickHouse/Git
	docHash := sha256.Sum256([]byte(payload.MarkdownBody))
	docHashHex := hex.EncodeToString(docHash[:])

	enrichedContent := fmt.Sprintf(
		"<!-- Auditado automaticamente via Fratuci Gemini Bridge | Autor: %s | SHA-256: %s -->\n%s",
		payload.AuthorRole,
		docHashHex,
		payload.MarkdownBody,
	)

	// 4. Comita o arquivo via GitHub Contents API na organização fratuci-labs
	commitURL, err := pushFileToGitHub(payload.TargetRepo, payload.TargetPath, payload.Branch, payload.CommitTitle, enrichedContent)
	if err != nil {
		log.Printf("[ERRO] Falha ao comitar no GitHub: %v", err)
		http.Error(w, "Erro ao gravar no GitHub: "+err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":       "committed",
		"organization": allowedGitHubOrg,
		"repository":   payload.TargetRepo,
		"path":         payload.TargetPath,
		"sha256":       docHashHex,
		"commit_url":   commitURL,
	})
}

func verifyHMAC(body []byte, signatureHeader, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signatureHeader))
}

func pushFileToGitHub(repo, path, branch, message, content string) (string, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return "", fmt.Errorf("GITHUB_TOKEN ausente no ambiente")
	}

	// Endpoint travado na organização fratuci-labs
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", allowedGitHubOrg, repo, path)

	reqBody, err := json.Marshal(map[string]string{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"branch":  branch,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPut, apiURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, string(respBytes))
	}

	var result struct {
		Commit struct {
			HTMLURL string `json:"html_url"`
		} `json:"commit"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&result)
	return result.Commit.HTMLURL, nil
}
