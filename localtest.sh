#!/bin/bash
set -e
#set -x

dir=$(dirname "$0")

mkdir $dir/coverage || true
sudo rm -f $dir/coverage/* || true

docker images alpine | grep alpine > /dev/null || docker pull alpine
docker build -t droutertest -f $dir/Dockertest $dir

if [[ -z "$@" ]]; then
	# test pid1
	docker run -it --name=drntest_drouter --privileged --rm -e TERM=xterm -e NO_TEST_SETUP=1 -e TEST_NO_HOST_PID=1 -v /var/run/docker.sock:/var/run/docker.sock -v $(pwd)/$dir/coverage:/coverage droutertest "go test github.com/TrilliumIT/docker-drouter/drouter -timeout 20m -coverprofile=/coverage/pid1.out -run=TestPid1"

	# test no docker socket
	docker run -it --name=drntest_drouter --privileged --rm --pid=host -e TERM=xterm -e NO_TEST_SETUP=1 -e TEST_NO_SOCKET=1 -v $(pwd)/$dir/coverage:/coverage droutertest "go test github.com/TrilliumIT/docker-drouter/drouter -timeout 20m -coverprofile=/coverage/nosocket.out -run=TestNoSocket"
fi

TEST_CMD="go test github.com/TrilliumIT/docker-drouter/drouter -timeout 20m -coverprofile=/coverage/main.out $@"
# test main
docker run -it --name=drntest_drouter --privileged --rm -e TERM=xterm --pid=host -v /var/run/docker.sock:/var/run/docker.sock -v $(pwd)/$dir/coverage:/coverage droutertest "$TEST_CMD"

# remove dangling images
[ -z $(docker images -q -f dangling=true | head -1) ] || docker rmi $(docker images -q -f dangling=true) > /dev/null

gocovmerge $dir/coverage/*.out > $dir/coverage/cover.out
if [ -z $TRAVIS_JOB_ID ] ; then
	echo "Press enter to view coverage"
	read
	go tool cover -html=$dir/coverage/cover.out
fi
