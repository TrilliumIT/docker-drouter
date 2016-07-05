#!/bin/sh
BASEDIR=$(realpath $(dirname "$0"))
if [ -e $BASEDIR/coverage/cover.out ]; then
	sudo rm -f $BASEDIR/coverage/cover.out
fi
docker images alpine | grep alpine > /dev/null || docker pull alpine
docker build -t droutertest -f $BASEDIR/Dockertest $BASEDIR
echo "$@"
docker run -it --name=drntest_drouter --privileged --rm -e TERM=xterm --pid=host -v /var/run/docker.sock:/var/run/docker.sock -v $BASEDIR/coverage:/coverage droutertest "go test github.com/TrilliumIT/docker-drouter/drouter -timeout 20m -coverprofile=/coverage/cover.out $@"
ec=$?
[ -z $(docker images -q -f dangling=true | head -1) ] || docker rmi $(docker images -q -f dangling=true)
if [ -e $BASEDIR/coverage/cover.out ]; then
	echo "Press enter to view coverage"
	read
	go tool cover -html=$BASEDIR/coverage/cover.out
fi
exit $ec
