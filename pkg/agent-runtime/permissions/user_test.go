package permissions

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestGetUser tests the GetUser function
func TestGetUser(t *testing.T) {
	// Test case: valid username
	t.Run("ValidUsername", func(t *testing.T) {
		// Arrange
		username := "root" // Assuming "root" exists on most systems

		// Act
		result, err := GetUser(username)

		// Assert
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.Equal(t, username, result.Username)
	})

	// Test case: invalid username
	t.Run("InvalidUsername", func(t *testing.T) {
		// Arrange
		username := "nonexistentuser"

		// Act
		result, err := GetUser(username)

		// Assert
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), fmt.Sprintf("error looking up user '%s'", username))
	})
}

// TestRealUserProvider_GetUser tests the GetUser method of RealUserProvider
func TestRealUserProvider_GetUser(t *testing.T) {
	// Test case: valid username
	t.Run("ValidUsername", func(t *testing.T) {
		// Arrange
		provider := &RealUserProvider{}
		username := "root" // Assuming "root" exists on most systems

		// Act
		result, err := provider.GetUser(username)

		// Assert
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.Equal(t, username, result.Username)
	})

	// Test case: invalid username
	t.Run("InvalidUsername", func(t *testing.T) {
		// Arrange
		provider := &RealUserProvider{}
		username := "nonexistentuser"

		// Act
		result, err := provider.GetUser(username)

		// Assert
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), fmt.Sprintf("error looking up user '%s'", username))
	})
}