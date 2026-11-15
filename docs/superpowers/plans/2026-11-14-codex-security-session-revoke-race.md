# Session revoke concurrency port plan

1. Confirm the Hosted patch's two existing files match Self-Hosted before applying it.
2. Port the handler and both focused regression files; update the direct test caller and changelog.
3. Run the Redis concurrency test and compile the sessions package locally. Run database-backed tests in isolated PR CI.
4. Publish a draft PR, clear CI and specialist review findings, then submit the ready PR for the required merge approval.
