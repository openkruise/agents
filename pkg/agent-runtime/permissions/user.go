package permissions

import (
	"fmt"
	"os/user"
)

func GetUser(username string) (u *user.User, err error) {
	u, err = user.Lookup(username)
	if err != nil {
		return nil, fmt.Errorf("error looking up user '%s': %w", username, err)
	}

	return u, nil
}

type RealUserProvider struct{}

func (r *RealUserProvider) GetUser(username string) (*user.User, error) {
	return GetUser(username)
}
