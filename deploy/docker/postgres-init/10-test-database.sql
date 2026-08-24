-- A separate database for the test suite.
--
-- Without this, `go test ./...` on a developer's machine writes into the same
-- database the local stack is using: test tenants accumulate in the dev
-- environment, and — worse — the platform-administrator seeding refuses to run
-- because tenants already exist, leaving the stack with no way to log in.
--
-- Both databases live in one container because starting two is not worth it for
-- a development stack; the isolation that matters is between the *data*.
CREATE DATABASE mcpdoll_test OWNER mcpdoll;
