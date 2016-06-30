BASEDIR=$(realpath $(dirname "$0"))
if [ -e $BASEDIR/coverage/cover.html ]; then
	sudo rm -f $BASEDIR/coverage/cover.html
fi
docker images alpine | grep alpine > /dev/null || docker pull alpine
docker build -t droutertest -f $BASEDIR/Dockertest $BASEDIR
docker run -it --name=drntest_drouter --privileged --rm --pid=host -v /var/run/docker.sock:/var/run/docker.sock -v $BASEDIR/coverage:/coverage droutertest 'go test github.com/TrilliumIT/docker-drouter/drouter -coverprofile=/coverage/cover.html'
ec=$?
docker rmi $(docker images -q -f dangling=true)
if [ -e $BASEDIR/coverage/cover.html ]; then
	echo "Press enter to view coverage"
	read
	go tool cover -html=$BASEDIR/coverage/cover.html
fi
exit $ec
