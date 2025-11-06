package writer

type notFoundError struct {
	err error
}

func (e notFoundError) Error() string {
	return e.err.Error()
}

func isNotFound(err error) bool {
	_, ok := err.(notFoundError)
	return ok
}

type alreadyExistError struct {
	err error
}

func (e alreadyExistError) Error() string {
	return e.err.Error()
}

func isAlreadyExists(err error) bool {
	_, ok := err.(alreadyExistError)
	return ok
}
