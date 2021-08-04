Forked On Aug-4-2021

# Changes
- Dockerfile is updated to extende from spark-hadoop image instead of just spark image

# Steps to create image

```
aws ecr-public get-login-password --region us-east-1 | docker login --username AWS --password-stdin public.ecr.aws/n6s8c1t1
docker build -t spark-operator-with-aws-jars .
docker tag spark-operator-with-aws-jars:latest public.ecr.aws/n6s8c1t1/spark-operator-with-aws-jars:latest
docker push public.ecr.aws/n6s8c1t1/spark-operator-with-aws-jars:latest
```