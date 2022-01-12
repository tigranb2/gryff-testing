timeout 180s bin/client -maddr=10.10.1.1 -writes=$1 -c=$2 -T=$3 -proxy
python3.8 client_metrics.py
