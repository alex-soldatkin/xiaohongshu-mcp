package cookies

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDomainMatches(t *testing.T) {
	assert.True(t, DomainMatches(".rednote.com", "rednote.com"))
	assert.True(t, DomainMatches("rednote.com", "rednote.com"))
	assert.True(t, DomainMatches("www.rednote.com", "rednote.com"))
	assert.True(t, DomainMatches("as.rednote.com", "rednote.com"))
	assert.False(t, DomainMatches("rednote.com.evil.test", "rednote.com"))
	assert.False(t, DomainMatches("www.xiaohongshu.com", "rednote.com"))
	assert.False(t, DomainMatches("", "rednote.com"))
}
