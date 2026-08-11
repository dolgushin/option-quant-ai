FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .

# Используем стандартный pypi с fallback на aliyun и увеличиваем retries/timeouts
RUN pip install --no-cache-dir \
    --timeout=120 \
    --retries=10 \
    --prefer-binary \
    --extra-index-url https://mirrors.aliyun.com/pypi/simple/ \
    -r requirements.txt

COPY . .

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "9000"]
