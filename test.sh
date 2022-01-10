timeout 60s bin/client -maddr=10.10.1.1 -writes=$1 -c=$2 -T=$3 
tail -n +200 latFileRead-0.txt > tmp.txt && mv tmp.txt latFileRead-0.txt
tail -n +200 latFileRead-1.txt > tmp.txt && mv tmp.txt latFileRead-1.txt
tail -n +200 latFileRead-2.txt > tmp.txt && mv tmp.txt latFileRead-2.txt
tail -n +200 latFileWrite-0.txt > tmp.txt && mv tmp.txt latFileWrite-0.txt
tail -n +200 latFileWrite-1.txt > tmp.txt && mv tmp.txt latFileWrite-1.txt
tail -n +200 latFileWrite-2.txt > tmp.txt && mv tmp.txt latFileWrite-2.txt
python3.8 client_metrics.py
