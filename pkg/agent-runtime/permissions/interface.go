package permissions

import (
	"os/user"
)

type UserProvider interface {
	GetUser(username string) (*user.User, error)
}

