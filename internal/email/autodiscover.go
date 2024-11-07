package email

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AutodiscoverXML represents the structure of an Autodiscover XML response
type AutodiscoverXML struct {
	XMLName  xml.Name `xml:"Autodiscover"`
	Response struct {
		Account struct {
			Protocol []struct {
				Type   string `xml:"Type"`
				Server string `xml:"Server"`
				Port   int    `xml:"Port"`
				SSL    bool   `xml:"SSL"`
			} `xml:"Protocol"`
		} `xml:"Account"`
	} `xml:"Response"`
}

func DetectMailServer(email string) (string, error) {
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid email format")
	}
	domain := parts[1]

	// First check if it's a common provider
	if server, ok := getCommonProvider(domain); ok {
		return server, nil
	}

	// Try autodiscover for custom domains
	server, err := tryAutodiscover(domain)
	if err == nil {
		return server, nil
	}

	// Fall back to standard IMAP server naming convention
	return fmt.Sprintf("imap.%s:993", domain), nil
}

func GetMailServer(email string) (string, error) {
	fmt.Println(email)
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid email format")
	}
	domain := parts[1]

	// First check if it's a common provider
	if server, ok := getCommonProvider(domain); ok {
		return server, nil
	}

	// If it's a custom domain, try autodiscover
	server, err := tryAutodiscover(domain)
	if err == nil {
		return server, nil
	}

	// Fall back to standard IMAP server naming convention
	return fmt.Sprintf("imap.%s:993", domain), nil
}

func getCommonProvider(domain string) (string, bool) {
	providers := map[string]string{
		"gmail.com":    "imap.gmail.com:993",
		"outlook.com":  "outlook.office365.com:993",
		"hotmail.com":  "outlook.office365.com:993",
		"live.com":     "outlook.office365.com:993",
		"yahoo.com":    "imap.mail.yahoo.com:993",
		"aol.com":      "imap.aol.com:993",
		"icloud.com":   "imap.mail.me.com:993",
		"fastmail.com": "imap.fastmail.com:993",
	}

	server, ok := providers[domain]
	return server, ok
}

func tryAutodiscover(domain string) (string, error) {
	// List of possible autodiscover URLs
	urls := []string{
		fmt.Sprintf("https://autodiscover.%s/autodiscover/autodiscover.xml", domain),
		fmt.Sprintf("https://%s/autodiscover/autodiscover.xml", domain),
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	for _, url := range urls {
		server, err := fetchAutodiscoverXML(client, url)
		if err == nil {
			return server, nil
		}
	}

	return "", fmt.Errorf("autodiscover failed for domain %s", domain)
}

func fetchAutodiscoverXML(client *http.Client, url string) (string, error) {
	// Create a request with basic auth placeholder (some servers require this)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth("autodiscover", "autodiscover")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("autodiscover request failed with status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var autodiscover AutodiscoverXML
	if err := xml.Unmarshal(body, &autodiscover); err != nil {
		return "", err
	}

	// Look for IMAP protocol settings
	for _, protocol := range autodiscover.Response.Account.Protocol {
		if strings.EqualFold(protocol.Type, "IMAP") {
			port := protocol.Port
			if port == 0 {
				if protocol.SSL {
					port = 993
				} else {
					port = 143
				}
			}
			return fmt.Sprintf("%s:%d", protocol.Server, port), nil
		}
	}

	return "", fmt.Errorf("no IMAP settings found in autodiscover response")
}
