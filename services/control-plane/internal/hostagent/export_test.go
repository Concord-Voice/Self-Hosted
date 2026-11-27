package hostagent

// Consume is the tests' view of consume, without the admin ID. Production
// code calls consume itself.
//
//nolint:revive // The closed job parser's actionRequest is intentionally package-private.
func (s *AuthorizationStore) Consume(handle, action string, assertion []byte) (actionRequest, error) {
	req, _, err := s.consume(handle, action, assertion)
	return req, err
}
