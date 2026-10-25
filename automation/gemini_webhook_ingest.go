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
	AuthorRole   string `json:"author_role"`       // Ex: "Adriana — Empreendedorismo & Farmácia" ou "André — Jurídico & Compliance"
	TargetRepo   string `json:"target_repo"`       // Ex: "strategy-and-healthtech" ou "legal-governance-as-code"
	TargetPath   string `json:"target_path"`       // Ex: "historico-gemini/2026-10-estudo-viabilidade-healthtech.md"
	CommitTitle  string `json:"commit_title"`      // Ex: "docs(healthtech): adiciona estudo de viabilidade via Gemini"
	MarkdownBody string `json:"markdown_body"`     // Conteúdo completo com Frontmatter YAML
	Branch       string `json:"branch"`            // Branch de destino (ex: "main" ou "feat/nova-regra-anvisa")
	BaseBranch   string `json:"base_branch"`       // Branch base para abertura de PR (padrão: "main")
	OpenPR       bool   `json:"open_pull_request"` // Se true, cria branch e abre Pull Request automaticamente
	PRTitle      string `json:"pr_title"`          // Título da Pull Request (opcional, usa CommitTitle como fallback)
	PRBody       string `json:"pr_body"`           // Corpo descritivo da Pull Request
}

const allowedGitHubOrg = "fratuci-labs"

var allowedRepositories = map[string]bool{
	"strategy-and-healthtech":  true,
	"legal-governance-as-code": true,
	"ai-agents-core":           true,
	"gcp-data-foundation":      true,
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("/v1/gemini-ingest", handleGeminiIngest)
	mux.HandleFunc("/v1/ingest", handleGeminiIngest)

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

	// 1. Validação de Assinatura Criptográfica HMAC-SHA256 (Fail-Closed)
	webhookSecret := os.Getenv("GEMINI_WEBHOOK_HMAC_SECRET")
	if webhookSecret == "" {
		http.Error(w, "Configuração de segurança ausente: GEMINI_WEBHOOK_HMAC_SECRET obrigatório", http.StatusInternalServerError)
		return
	}
	sigHeader := r.Header.Get("X-Fratuci-Signature-256")
	if !verifyHMAC(bodyBytes, sigHeader, webhookSecret) {
		http.Error(w, "Assinatura HMAC inválida", http.StatusUnauthorized)
		return
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
	if strings.Contains(payload.TargetPath, "..") || strings.HasPrefix(payload.TargetPath, "/") || strings.TrimSpace(payload.TargetPath) == "" {
		http.Error(w, "Caminho de arquivo inválido", http.StatusBadRequest)
		return
	}
	if payload.BaseBranch == "" {
		payload.BaseBranch = "main"
	}
	if payload.Branch == "" {
		if payload.OpenPR {
			payload.Branch = fmt.Sprintf("gemini-auto/%d", time.Now().UTC().Unix())
		} else {
			payload.Branch = payload.BaseBranch
		}
	}
	if payload.OpenPR && payload.Branch == payload.BaseBranch {
		payload.Branch = fmt.Sprintf("gemini-auto/%d", time.Now().UTC().Unix())
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

	// 4. Se for fluxo de Pull Request (ou branch dedicada), garante a criação da branch a partir da base
	if payload.Branch != payload.BaseBranch {
		if err := ensureBranchExists(payload.TargetRepo, payload.BaseBranch, payload.Branch); err != nil {
			log.Printf("[ERRO] Falha ao preparar branch '%s': %v", payload.Branch, err)
			http.Error(w, "Erro ao preparar branch no GitHub: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	// 5. Comita o arquivo (criação ou atualização com SHA prévio) via GitHub Contents API
	commitURL, err := pushFileToGitHub(payload.TargetRepo, payload.TargetPath, payload.Branch, payload.CommitTitle, enrichedContent)
	if err != nil {
		log.Printf("[ERRO] Falha ao comitar no GitHub: %v", err)
		http.Error(w, "Erro ao gravar no GitHub: "+err.Error(), http.StatusBadGateway)
		return
	}

	responsePayload := map[string]string{
		"status":       "committed",
		"organization": allowedGitHubOrg,
		"repository":   payload.TargetRepo,
		"branch":       payload.Branch,
		"path":         payload.TargetPath,
		"sha256":       docHashHex,
		"commit_url":   commitURL,
	}

	// 6. Abre Pull Request automaticamente caso solicitado no payload
	if payload.OpenPR {
		prTitle := payload.PRTitle
		if prTitle == "" {
			prTitle = payload.CommitTitle
		}
		prBody := payload.PRBody
		if prBody == "" {
			prBody = fmt.Sprintf(
				"> 👤 **[%s]**\n\nArtefato gerado via conversa no **Google Gemini App / Spark** e enviado automaticamente pela ponte `gemini-webhook-ingest`.\n\n- **Arquivo:** `%s`\n- **SHA-256 do Conteúdo:** `%s`",
				payload.AuthorRole,
				payload.TargetPath,
				docHashHex,
			)
		}
		prURL, err := createPullRequestInGitHub(payload.TargetRepo, payload.Branch, payload.BaseBranch, prTitle, prBody)
		if err != nil {
			log.Printf("[ERRO] Commit realizado, mas falha ao abrir Pull Request: %v", err)
			http.Error(w, "Erro ao abrir Pull Request no GitHub: "+err.Error(), http.StatusBadGateway)
			return
		}
		responsePayload["status"] = "pull_request_opened"
		responsePayload["pull_request_url"] = prURL
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(responsePayload)
}

func verifyHMAC(body []byte, signatureHeader, secret string) bool {
	if signatureHeader == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signatureHeader))
}

func ensureBranchExists(repo, baseBranch, targetBranch string) error {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return fmt.Errorf("GITHUB_TOKEN ausente no ambiente")
	}

	// Verifica se a branch de destino já existe
	targetRefURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/ref/heads/%s", allowedGitHubOrg, repo, targetBranch)
	reqCheck, err := newGitHubRequest(http.MethodGet, targetRefURL, token, nil)
	if err != nil {
		return err
	}
	respCheck, err := httpClient.Do(reqCheck)
	if err != nil {
		return err
	}
	defer respCheck.Body.Close()
	if respCheck.StatusCode == http.StatusOK {
		return nil
	}

	// Obtém o SHA da branch base
	baseRefURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/ref/heads/%s", allowedGitHubOrg, repo, baseBranch)
	reqBase, err := newGitHubRequest(http.MethodGet, baseRefURL, token, nil)
	if err != nil {
		return err
	}
	respBase, err := httpClient.Do(reqBase)
	if err != nil {
		return err
	}
	defer respBase.Body.Close()
	if respBase.StatusCode >= 300 {
		b, _ := io.ReadAll(respBase.Body)
		return fmt.Errorf("falha ao consultar branch base '%s' (status %d): %s", baseBranch, respBase.StatusCode, string(b))
	}

	var baseRef struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.NewDecoder(respBase.Body).Decode(&baseRef); err != nil {
		return err
	}

	// Cria a nova branch na organização fratuci-labs
	createRefURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/refs", allowedGitHubOrg, repo)
	createBody, err := json.Marshal(map[string]string{
		"ref": "refs/heads/" + targetBranch,
		"sha": baseRef.Object.SHA,
	})
	if err != nil {
		return err
	}
	reqCreate, err := newGitHubRequest(http.MethodPost, createRefURL, token, bytes.NewReader(createBody))
	if err != nil {
		return err
	}
	respCreate, err := httpClient.Do(reqCreate)
	if err != nil {
		return err
	}
	defer respCreate.Body.Close()
	if respCreate.StatusCode >= 300 {
		b, _ := io.ReadAll(respCreate.Body)
		return fmt.Errorf("falha ao criar branch '%s' (status %d): %s", targetBranch, respCreate.StatusCode, string(b))
	}
	return nil
}

func pushFileToGitHub(repo, path, branch, message, content string) (string, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return "", fmt.Errorf("GITHUB_TOKEN ausente no ambiente")
	}

	// Endpoint travado na organização fratuci-labs
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", allowedGitHubOrg, repo, path)

	// Consulta se o arquivo já existe na branch para obter o blob SHA (necessário para atualização)
	var existingSHA string
	reqGet, err := newGitHubRequest(http.MethodGet, apiURL+"?ref="+branch, token, nil)
	if err == nil {
		if respGet, errGet := httpClient.Do(reqGet); errGet == nil {
			defer respGet.Body.Close()
			if respGet.StatusCode == http.StatusOK {
				var existingFile struct {
					SHA string `json:"sha"`
				}
				_ = json.NewDecoder(respGet.Body).Decode(&existingFile)
				existingSHA = existingFile.SHA
			}
		}
	}

	payloadMap := map[string]string{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"branch":  branch,
	}
	if existingSHA != "" {
		payloadMap["sha"] = existingSHA
	}

	reqBody, err := json.Marshal(payloadMap)
	if err != nil {
		return "", err
	}

	req, err := newGitHubRequest(http.MethodPut, apiURL, token, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}

	resp, err := httpClient.Do(req)
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

func createPullRequestInGitHub(repo, headBranch, baseBranch, title, body string) (string, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return "", fmt.Errorf("GITHUB_TOKEN ausente no ambiente")
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls", allowedGitHubOrg, repo)
	reqBody, err := json.Marshal(map[string]string{
		"title": title,
		"head":  headBranch,
		"base":  baseBranch,
		"body":  body,
	})
	if err != nil {
		return "", err
	}

	req, err := newGitHubRequest(http.MethodPost, apiURL, token, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, string(respBytes))
	}

	var prResult struct {
		HTMLURL string `json:"html_url"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&prResult)
	return prResult.HTMLURL, nil
}

func newGitHubRequest(method, url, token string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}
