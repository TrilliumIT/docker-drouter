BASEDIR=$(dirname "$0")
docker build -t droutertest -f $BASEDIR/Dockertest $BASEDIR
docker run -it --privileged --rm droutertest "go test $($GOPATH/bin/glide novendor)"
ec=$?
docker rmi droutertest > /dev/null
exit $ec
