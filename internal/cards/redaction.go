package cards

import (
	"regexp"
	"strings"
)

// Match common unquoted email addresses, including plus aliases and subdomains.
// This is display redaction, not an RFC mailbox parser or a DLP guarantee.
const emailAddressPattern = `[A-Za-z0-9_%+\-]+(?:\.[A-Za-z0-9_%+\-]+)*@[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?)*\.[A-Za-z]{2,}`

var emailAddress = regexp.MustCompile(emailAddressPattern)

// Include the surrounding email link so masking its target cannot leave an
// unusable mailto URL. The splitter keeps each display token intact as well.
var publicEmail = regexp.MustCompile(`\[[^\]\r\n]*\]\((?i:mailto:)` + emailAddressPattern + `(?:\?[^)\r\n]*)?\)|<(?i:mailto:)?` + emailAddressPattern + `>|(?i:mailto:)?` + emailAddressPattern)

func redactEmails(text string) string {
	mask := func(address string) string { return strings.ReplaceAll(address, "@", "<A>") }
	return publicEmail.ReplaceAllStringFunc(text, func(match string) string {
		if strings.HasPrefix(match, "[") {
			label, target, _ := strings.Cut(match[1:], "](")
			label = emailAddress.ReplaceAllStringFunc(label, mask)
			address := mask(emailAddress.FindString(target))
			if label != "" && label != address {
				return label + " " + address
			}
			return address
		}
		return mask(emailAddress.FindString(match))
	})
}
